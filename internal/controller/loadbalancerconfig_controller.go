// Package controller implements the LoadBalancerConfig reconciler that manages
// Huawei Cloud ELBs for OpenEverest V1.
//
// This controller watches LoadBalancerConfig CRs (everest.percona.com/v1alpha1)
// that carry the label `huawei-elb.io/controlled=true`. For each such CR, it:
//
//  1. Reads ELB creation parameters from metadata.annotations (huawei-elb.io/*).
//  2. Creates a Huawei Cloud ELB via the ELB v3 API.
//  3. Writes the ELB ID back into spec.annotations["kubernetes.io/elb.id"] so
//     that the OpenEverest V1 operator — which reads spec.annotations and puts
//     them onto the K8s LoadBalancer Service — causes CCE CCM to bind the
//     pre-created ELB.
//  4. Sets metadata.annotations["huawei-elb.io/ready"]="true" once the ELB is
//     ACTIVE, allowing users/V1 to wait before creating DatabaseCluster CRs.
//  5. On deletion, removes the ELB via the API before releasing the CR.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

const (
	// finalizerName ensures the ELB is deleted before the LoadBalancerConfig CR.
	finalizerName = "huawei-elb.io/finalizer"

	// controlledLabel marks a LoadBalancerConfig as managed by this controller.
	controlledLabel = "huawei-elb.io/controlled"

	// readyAnnotation is set to "true" once the ELB is ACTIVE, "false" otherwise.
	// Users can wait on this before creating DatabaseCluster CRs:
	//   kubectl wait loadbalancerconfig <name> --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true
	readyAnnotation = "huawei-elb.io/ready"

	// errorAnnotation records the last reconciliation error (empty when healthy).
	errorAnnotation = "huawei-elb.io/error"

	// Requeue intervals
	provisioningRequeue = 30 * time.Second // ELB not yet ACTIVE
	healthyRequeue      = 5 * time.Minute  // periodic health check when ACTIVE
	errorRequeue        = 5 * time.Minute  // permanent errors (bad params, etc.)
	retryRequeue        = 10 * time.Second // temporary errors (network, throttling)
)

// lbcGVR is the GroupVersionKind for OpenEverest V1's LoadBalancerConfig CR.
var lbcGVR = schema.GroupVersionKind{
	Group:   "everest.percona.com",
	Version: "v1alpha1",
	Kind:    "LoadBalancerConfig",
}

// LoadBalancerConfigReconciler reconciles LoadBalancerConfig CRs and manages
// the corresponding Huawei Cloud ELBs.
type LoadBalancerConfigReconciler struct {
	client.Client
	ELBClient *elb.ElbClient
	// Creds holds the default Huawei Cloud credentials. Used to create
	// per-LBC ELB clients when the CR specifies a different region via
	// the "huawei-elb.io/region" annotation.
	Creds *huaweicloud.Credentials
}

// getELBClient returns the ELB client for the given LoadBalancerConfig.
// If the CR specifies a different region via the "huawei-elb.io/region"
// annotation, a new client is created for that region (using the same
// AK/SK/ProjectID from the default credentials).
func (r *LoadBalancerConfigReconciler) getELBClient(lbc *unstructured.Unstructured) (*elb.ElbClient, error) {
	region := lbc.GetAnnotations()["huawei-elb.io/region"]
	if region == "" || region == r.Creds.Region {
		return r.ELBClient, nil
	}
	return huaweicloud.NewELBClient(&huaweicloud.Credentials{
		AK:        r.Creds.AK,
		SK:        r.Creds.SK,
		Region:    region,
		ProjectID: r.Creds.ProjectID,
	})
}

// Reconcile is the main reconcile loop for a LoadBalancerConfig.
func (r *LoadBalancerConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	lbc, err := r.getLoadBalancerConfig(ctx, req.NamespacedName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion — always process if we have a finalizer, even if the
	// controlled label was removed by the user after creation.
	if !lbc.GetDeletionTimestamp().IsZero() {
		if controllerutil.ContainsFinalizer(lbc, finalizerName) {
			return r.reconcileDelete(ctx, logger, lbc)
		}
		return ctrl.Result{}, nil
	}

	// Skip CRs not marked as controlled by us (non-deletion path only).
	if !isControlled(lbc) {
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling LoadBalancerConfig", "name", lbc.GetName())

	// Ensure finalizer is present before doing anything else.
	if !controllerutil.ContainsFinalizer(lbc, finalizerName) {
		if err := r.updateWithRetry(ctx, req.NamespacedName, func(latest *unstructured.Unstructured) error {
			controllerutil.AddFinalizer(latest, finalizerName)
			return nil
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileEnsure(ctx, logger, lbc)
}

// reconcileEnsure creates or verifies the ELB for the given LoadBalancerConfig.
func (r *LoadBalancerConfigReconciler) reconcileEnsure(
	ctx context.Context, logger logr.Logger, lbc *unstructured.Unstructured,
) (ctrl.Result, error) {
	elbClient, err := r.getELBClient(lbc)
	if err != nil {
		return r.handleTransientError(ctx, lbc, logger, fmt.Errorf("creating per-region ELB client: %w", err))
	}
	elbID := getSpecAnnotation(lbc, huaweicloud.AnnotationELBID)

	// Case 1: No ELB ID in annotations yet — create the ELB.
	if elbID == "" {
		// Mark as not ready while we create the ELB.
		_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")

		// Idempotency: check if an ELB already exists by name (e.g. after a crash).
		elbName := huaweicloud.ELBNamePrefix + lbc.GetName()
		existing, err := huaweicloud.FindELBByName(elbClient, elbName)
		if err != nil {
			return r.handleTransientError(ctx, lbc, logger, fmt.Errorf("checking for existing ELB %q: %w", elbName, err))
		}
		if existing != nil {
			logger.Info("Found existing ELB by name, recording ID", "elbID", existing.ID)
			if err := r.setSpecAnnotation(ctx, lbc, huaweicloud.AnnotationELBID, existing.ID); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: provisioningRequeue}, nil
		}

		// Parse parameters and create a new ELB.
		opt, err := parseELBOptions(lbc)
		if err != nil {
			return r.handlePermanentError(ctx, lbc, logger, err)
		}

		logger.Info("Creating Huawei Cloud ELB", "name", opt.Name, "public", opt.IsPublic)
		info, err := huaweicloud.CreateELB(elbClient, *opt)
		if err != nil {
			return r.handleTransientError(ctx, lbc, logger, fmt.Errorf("creating ELB: %w", err))
		}

		logger.Info("ELB created", "elbID", info.ID, "status", info.ProvisioningStatus)
		_ = r.setAnnotation(ctx, lbc, errorAnnotation, "")
		if err := r.setSpecAnnotation(ctx, lbc, huaweicloud.AnnotationELBID, info.ID); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: provisioningRequeue}, nil
	}

	// Case 2: ELB ID is present — verify status.
	info, err := huaweicloud.ShowELB(elbClient, elbID)
	if err != nil {
		return r.handleTransientError(ctx, lbc, logger, fmt.Errorf("showing ELB %q: %w", elbID, err))
	}

	logger.V(1).Info("ELB status", "elbID", elbID,
		"provisioning", info.ProvisioningStatus, "operating", info.OperatingStatus)

	// Record public IP if available (useful for debugging, stored in metadata).
	if info.PublicIP != "" {
		_ = r.setAnnotation(ctx, lbc, "huawei-elb.io/public-ip", info.PublicIP)
	}
	_ = r.setAnnotation(ctx, lbc, "huawei-elb.io/elb-status", info.ProvisioningStatus)

	// Set ready annotation based on ELB status.
	if info.ProvisioningStatus == "ACTIVE" {
		_ = r.setAnnotation(ctx, lbc, readyAnnotation, "true")
		_ = r.setAnnotation(ctx, lbc, errorAnnotation, "")
		return ctrl.Result{RequeueAfter: healthyRequeue}, nil
	}

	_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")
	return ctrl.Result{RequeueAfter: provisioningRequeue}, nil
}

// reconcileDelete deletes the ELB and removes the finalizer.
func (r *LoadBalancerConfigReconciler) reconcileDelete(
	ctx context.Context, logger logr.Logger, lbc *unstructured.Unstructured,
) (ctrl.Result, error) {
	elbClient, err := r.getELBClient(lbc)
	if err != nil {
		return r.handleTransientError(ctx, lbc, logger, fmt.Errorf("creating per-region ELB client: %w", err))
	}

	// Mark as not ready during deletion.
	_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")
	// Mark as not ready during deletion.
	_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")

	elbID := getSpecAnnotation(lbc, huaweicloud.AnnotationELBID)
	if elbID != "" {
		logger.Info("Deleting Huawei Cloud ELB", "elbID", elbID)
		if err := huaweicloud.DeleteELB(elbClient, elbID); err != nil {
			// If the ELB is already gone, proceed with finalizer removal.
			if !strings.Contains(err.Error(), "not found") {
				return r.handleTransientError(ctx, lbc, logger, fmt.Errorf("deleting ELB %q: %w", elbID, err))
			}
			logger.Info("ELB already deleted, proceeding", "elbID", elbID)
		}
	}

	// Remove finalizer with conflict retry.
	name := client.ObjectKeyFromObject(lbc)
	if err := r.updateWithRetry(ctx, name, func(latest *unstructured.Unstructured) error {
		controllerutil.RemoveFinalizer(latest, finalizerName)
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	logger.Info("LoadBalancerConfig deleted, ELB cleaned up", "name", lbc.GetName())
	return ctrl.Result{}, nil
}

// --- Error Handling ---

// handlePermanentError logs the error, records it in annotations, and returns
// a long requeue interval. Permanent errors are caused by invalid user input
// (e.g. missing annotations) and won't resolve without user action.
func (r *LoadBalancerConfigReconciler) handlePermanentError(
	ctx context.Context, lbc *unstructured.Unstructured, logger logr.Logger, err error,
) (ctrl.Result, error) {
	logger.Error(err, "permanent error, will retry in 5 minutes")
	_ = r.setAnnotation(ctx, lbc, errorAnnotation, err.Error())
	_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")
	return ctrl.Result{RequeueAfter: errorRequeue}, nil
}

// handleTransientError logs the error, records it in annotations, and returns
// a short requeue interval. Transient errors are caused by network issues,
// API throttling, or temporary ELB states.
func (r *LoadBalancerConfigReconciler) handleTransientError(
	ctx context.Context, lbc *unstructured.Unstructured, logger logr.Logger, err error,
) (ctrl.Result, error) {
	logger.Error(err, "transient error, will retry shortly")
	_ = r.setAnnotation(ctx, lbc, errorAnnotation, err.Error())
	_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")
	return ctrl.Result{RequeueAfter: retryRequeue}, nil
}

// --- Helpers ---

// getLoadBalancerConfig fetches the LoadBalancerConfig CR as an unstructured object.
func (r *LoadBalancerConfigReconciler) getLoadBalancerConfig(
	ctx context.Context, name types.NamespacedName,
) (*unstructured.Unstructured, error) {
	lbc := &unstructured.Unstructured{}
	lbc.SetGroupVersionKind(lbcGVR)
	if err := r.Get(ctx, name, lbc); err != nil {
		return nil, err
	}
	return lbc, nil
}

// isControlled returns true if the CR has the controlled label set to "true".
func isControlled(lbc *unstructured.Unstructured) bool {
	return lbc.GetLabels()[controlledLabel] == "true"
}

// getSpecAnnotation reads a single key from spec.annotations.
func getSpecAnnotation(lbc *unstructured.Unstructured, key string) string {
	anns, found, _ := unstructured.NestedStringMap(lbc.Object, "spec", "annotations")
	if !found {
		return ""
	}
	return anns[key]
}

// updateWithRetry retries the update on conflict by re-getting the latest version.
// The modifyFn is called with the latest version of the CR; it should modify
// the object in place (not call Update).
func (r *LoadBalancerConfigReconciler) updateWithRetry(
	ctx context.Context, name types.NamespacedName, modifyFn func(*unstructured.Unstructured) error,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(lbcGVR)
		if err := r.Get(ctx, name, latest); err != nil {
			return err
		}
		if err := modifyFn(latest); err != nil {
			return err
		}
		return r.Update(ctx, latest)
	})
}

// setSpecAnnotation writes a key-value pair into spec.annotations and persists
// with conflict retry.
func (r *LoadBalancerConfigReconciler) setSpecAnnotation(
	ctx context.Context, lbc *unstructured.Unstructured, key, value string,
) error {
	name := client.ObjectKeyFromObject(lbc)
	return r.updateWithRetry(ctx, name, func(latest *unstructured.Unstructured) error {
		anns, _, _ := unstructured.NestedStringMap(latest.Object, "spec", "annotations")
		if anns == nil {
			anns = map[string]string{}
		}
		anns[key] = value
		return unstructured.SetNestedStringMap(latest.Object, anns, "spec", "annotations")
	})
}

// setAnnotation writes (or removes when value is empty) a metadata annotation
// and persists with conflict retry. Errors are non-fatal — callers typically
// use _ = r.setAnnotation(...).
func (r *LoadBalancerConfigReconciler) setAnnotation(
	ctx context.Context, lbc *unstructured.Unstructured, key, value string,
) error {
	name := client.ObjectKeyFromObject(lbc)
	return r.updateWithRetry(ctx, name, func(latest *unstructured.Unstructured) error {
		anns := latest.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		if value == "" {
			// Only update if the key exists.
			if _, ok := anns[key]; !ok {
				return nil
			}
			delete(anns, key)
		} else {
			if anns[key] == value {
				return nil // no change
			}
			anns[key] = value
		}
		latest.SetAnnotations(anns)
		return nil
	})
}

// parseELBOptions reads ELB creation parameters from metadata.annotations.
//
// Required annotations:
//   - huawei-elb.io/vpc-id
//   - huawei-elb.io/subnet-id
//   - huawei-elb.io/availability-zones (comma-separated)
//
// Optional annotations (for public ELB):
//   - huawei-elb.io/public: "true" (default "false")
//   - huawei-elb.io/bandwidth-size: e.g. "20" (default 10)
//   - huawei-elb.io/bandwidth-charge-mode: "traffic" or "bandwidth" (default "traffic")
//   - huawei-elb.io/public-ip-network-type: e.g. "5_bgp" (default "5_bgp")
func parseELBOptions(lbc *unstructured.Unstructured) (*huaweicloud.CreateELBOption, error) {
	a := lbc.GetAnnotations()

	vpcID := a["huawei-elb.io/vpc-id"]
	subnetID := a["huawei-elb.io/subnet-id"]
	azStr := a["huawei-elb.io/availability-zones"]

	if vpcID == "" || subnetID == "" || azStr == "" {
		return nil, fmt.Errorf(
			"missing required annotations: huawei-elb.io/vpc-id, " +
				"huawei-elb.io/subnet-id, huawei-elb.io/availability-zones",
		)
	}

	azs := strings.Split(azStr, ",")
	for i := range azs {
		azs[i] = strings.TrimSpace(azs[i])
	}

	opt := &huaweicloud.CreateELBOption{
		Name:                 huaweicloud.ELBNamePrefix + lbc.GetName(),
		VpcID:                vpcID,
		VipSubnetCidrID:      subnetID,
		AvailabilityZoneList: azs,
		Tags: map[string]string{
			"managed-by": "huawei-elb-controller",
			"lbc-name":   lbc.GetName(),
		},
	}

	if strings.EqualFold(a["huawei-elb.io/public"], "true") {
		opt.IsPublic = true
		if bw, err := strconv.Atoi(a["huawei-elb.io/bandwidth-size"]); err == nil && bw > 0 {
			opt.BandwidthSize = int32(bw)
		}
		opt.BandwidthChargeMode = a["huawei-elb.io/bandwidth-charge-mode"]
		opt.PublicIPNetworkType = a["huawei-elb.io/public-ip-network-type"]
	}

	return opt, nil
}

// SetupWithManager registers the controller with the manager.
// A predicate filters events so only CRs with the controlled label are processed.
// Delete events are always processed to ensure finalizer cleanup even if the
// label was removed.
func (r *LoadBalancerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	lbc := &unstructured.Unstructured{}
	lbc.SetGroupVersionKind(lbcGVR)

	controlledPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetLabels()[controlledLabel] == "true"
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Process if either old or new object has the label — covers
			// label addition and removal.
			return e.ObjectOld.GetLabels()[controlledLabel] == "true" ||
				e.ObjectNew.GetLabels()[controlledLabel] == "true"
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Always process deletes to clean up finalizers.
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return e.Object.GetLabels()[controlledLabel] == "true"
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(lbc).
		WithEventFilter(controlledPredicate).
		Complete(r)
}
