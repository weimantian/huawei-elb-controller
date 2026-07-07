// Package controller implements the LoadBalancerConfig reconciler that manages
// Huawei Cloud ELBs for OpenEverest.
//
// This controller watches LoadBalancerConfig CRs (everest.percona.com/v1alpha1).
// If the CR has huawei-elb.io/* annotations in spec.annotations, it uses those
// parameters. If the annotations are missing, it auto-detects VPC/subnet/AZ
// from the cluster's nodes (zero-config, similar to EKS/GKE) and stores them in
// metadata.annotations (NOT spec.annotations) to avoid conflicts with the
// OpenEverest UI which edits spec.annotations. For each CR, it:
//
//  1. Reads or auto-detects ELB creation parameters (VPC, subnet, AZs).
//  2. Creates a Huawei Cloud ELB via the ELB v3 API.
//  3. Writes the ELB ID back into spec.annotations["kubernetes.io/elb.id"] so
//     that the OpenEverest operator — which reads spec.annotations and puts
//     them onto the K8s LoadBalancer Service — causes CCE CCM to bind the
//     pre-created ELB.
//  4. Sets metadata.annotations["huawei-elb.io/ready"]="true" once the ELB is
//     ACTIVE, allowing users to wait before creating DatabaseCluster CRs.
//  5. On deletion, removes the ELB via the API before releasing the CR.
package controller

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	corev1 "k8s.io/api/core/v1"
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

	// readyAnnotation is set to "true" once the ELB is ACTIVE, "false" otherwise.
	// Users can wait on this before creating DatabaseCluster CRs:
	//   kubectl wait loadbalancerconfig <name> --for=jsonpath='{.metadata.annotations.huawei-elb\.io/ready}'=true
	readyAnnotation = "huawei-elb.io/ready"

	// errorAnnotation records the last reconciliation error (empty when healthy).
	errorAnnotation = "huawei-elb.io/error"
	// Annotation keys for ELB creation parameters (spec.annotations or metadata.annotations).
	vpcIDAnnotation               = "huawei-elb.io/vpc-id"
	subnetIDAnnotation            = "huawei-elb.io/subnet-id"
	availabilityZonesAnnotation   = "huawei-elb.io/availability-zones"
	autoDetectedAnnotation        = "huawei-elb.io/auto-detected"
	regionAnnotation              = "huawei-elb.io/region"
	publicAnnotation              = "huawei-elb.io/public"
	bandwidthSizeAnnotation       = "huawei-elb.io/bandwidth-size"
	bandwidthChargeModeAnnotation = "huawei-elb.io/bandwidth-charge-mode"
	publicIPNetworkTypeAnnotation = "huawei-elb.io/public-ip-network-type"

	// Annotation keys for controller-written status (metadata.annotations).
	elbStatusAnnotation = "huawei-elb.io/elb-status"
	publicIPAnnotation  = "huawei-elb.io/public-ip"

	// CCM annotation for native CCE autocreate (skip reconciliation).
	ccmAutocreateAnnotation = "kubernetes.io/elb.autocreate"

	// Requeue intervals
	provisioningRequeue = 30 * time.Second // ELB not yet ACTIVE
	healthyRequeue      = 5 * time.Minute  // periodic health check when ACTIVE
	errorRequeue        = 5 * time.Minute  // permanent errors (bad params, etc.)
	retryRequeue        = 10 * time.Second // temporary errors (network, throttling)

	// uiGracePeriod is the minimum age of a LoadBalancerConfig before the
	// controller modifies it. This gives the OpenEverest UI time to complete
	// post-create operations (reload, update) without resourceVersion conflicts.
	uiGracePeriod = 5 * time.Second
)

// foreignCloudAnnotationPrefixes are spec.annotation key prefixes that
// indicate the LBC targets a different cloud provider (AWS, GKE, Azure,
// Alibaba). The controller skips these to avoid creating Huawei Cloud ELBs
// for LBCs meant for other clouds (e.g. OpenEverest's built-in eks-default).
var foreignCloudAnnotationPrefixes = []string{
	"service.beta.kubernetes.io/aws-",
	"service.beta.kubernetes.io/azure-",
	"service.beta.kubernetes.io/alibaba-",
	"cloud.google.com/",
	"networking.gke.io/",
}

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

	// Auto-detection cache. On CCE, all nodes share the same VPC/subnet,
	// so we only need to detect once and cache the result.
	detectMu sync.Mutex
	detected *autoDetectedParams
}

// autoDetectedParams holds the cluster's VPC/subnet/AZ detected from nodes.
type autoDetectedParams struct {
	VPCID    string
	SubnetID string
	AZs      []string
}

// getELBClient returns the ELB client for the given LoadBalancerConfig.
// If the CR specifies a different region via the "huawei-elb.io/region"
// annotation, a new client is created for that region (using the same
// AK/SK/ProjectID from the default credentials).
func (r *LoadBalancerConfigReconciler) getELBClient(lbc *unstructured.Unstructured) (*elb.ElbClient, error) {
	region := getSpecAnnotation(lbc, regionAnnotation)
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

	// If the LBC is not controlled (no huawei-elb.io/vpc-id in spec.annotations),
	// try to auto-detect VPC/subnet/AZ from cluster nodes. This gives a zero-config
	// experience on CCE — users just create a LoadBalancerConfig with a name and
	// the controller figures out the rest, similar to EKS/GKE.
	if !isControlled(lbc) {
		// Skip if using CCM autocreate (user chose the native CCE path).
		if getSpecAnnotation(lbc, ccmAutocreateAnnotation) != "" {
			return ctrl.Result{}, nil
		}

		// Skip LBCs that target a different cloud provider (AWS, GKE, Azure, etc.).
		// OpenEverest ships an eks-default LBC template with aws-load-balancer-type
		// annotation; without this filter the controller would create a Huawei Cloud
		// ELB for it.
		if hasForeignCloudAnnotations(lbc) {
			return ctrl.Result{}, nil
		}

		// If ELB ID already exists and we have a finalizer, monitor it
		// (could be a legacy LBC created by an older controller version).
		if getSpecAnnotation(lbc, huaweicloud.AnnotationELBID) != "" {
			if controllerutil.ContainsFinalizer(lbc, finalizerName) {
				return r.reconcileEnsure(ctx, logger, lbc)
			}
			return ctrl.Result{}, nil
		}
		// Grace period: if the LBC was created very recently, wait before
		// modifying it. The OpenEverest UI performs post-create operations
		// (reload, update) that conflict with controller writes.
		if age := time.Since(lbc.GetCreationTimestamp().Time); age < uiGracePeriod {
			logger.Info("LBC recently created, waiting to avoid UI conflict",
				"age", age, "wait", uiGracePeriod-age)
			return ctrl.Result{RequeueAfter: uiGracePeriod - age}, nil
		}

		// Auto-detect VPC/subnet/AZ from cluster nodes.
		vpcID, subnetID, azs, err := r.autoDetectParams(ctx, logger, lbc)
		if err != nil {
			logger.Error(err, "auto-detection failed")
			if isInUse(lbc) {
				errMsg := fmt.Sprintf("auto-detection failed: %v", err)
				anns := lbc.GetAnnotations()
				if anns[errorAnnotation] != errMsg {
					_ = r.setAnnotation(ctx, lbc, errorAnnotation, errMsg)
				}
				if anns[readyAnnotation] != "false" {
					_ = r.setAnnotation(ctx, lbc, readyAnnotation, "false")
				}
			}
			return ctrl.Result{RequeueAfter: errorRequeue}, nil
		}

		// Write detected values into metadata.annotations (NOT spec.annotations)
		// to avoid resourceVersion conflicts with the OpenEverest UI which edits
		// spec.annotations. The ELB ID (kubernetes.io/elb.id) is the only key
		// written to spec.annotations, and that happens once after ELB creation.
		logger.Info("Auto-detected VPC/subnet/AZ from cluster nodes",
			"vpc-id", vpcID, "subnet-id", subnetID, "azs", azs)

		// Write auto-detected params, finalizer, and ready=false in a single
		// update to minimize resourceVersion changes that conflict with the UI.
		if err := r.updateWithRetry(ctx, req.NamespacedName, func(latest *unstructured.Unstructured) error {
			anns := latest.GetAnnotations()
			if anns == nil {
				anns = map[string]string{}
			}
			anns[vpcIDAnnotation] = vpcID
			anns[subnetIDAnnotation] = subnetID
			anns[availabilityZonesAnnotation] = strings.Join(azs, ",")
			anns[autoDetectedAnnotation] = "true"
			anns[readyAnnotation] = "false"
			latest.SetAnnotations(anns)
			controllerutil.AddFinalizer(latest, finalizerName)
			return nil
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("writing auto-detected params and finalizer: %w", err)
		}

		return ctrl.Result{Requeue: true}, nil
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
		// Write ELB ID (spec.annotations) and clear error (metadata.annotations)
		// in a single update to minimize resourceVersion changes.
		if err := r.updateWithRetry(ctx, client.ObjectKeyFromObject(lbc), func(latest *unstructured.Unstructured) error {
			specAnns, _, _ := unstructured.NestedStringMap(latest.Object, "spec", "annotations")
			if specAnns == nil {
				specAnns = map[string]string{}
			}
			specAnns[huaweicloud.AnnotationELBID] = info.ID
			if err := unstructured.SetNestedStringMap(latest.Object, specAnns, "spec", "annotations"); err != nil {
				return err
			}
			anns := latest.GetAnnotations()
			if anns == nil {
				anns = map[string]string{}
			}
			anns[errorAnnotation] = ""
			latest.SetAnnotations(anns)
			return nil
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("writing ELB ID: %w", err)
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

	// Update all status annotations in a single write to minimize
	// resourceVersion changes that could conflict with the OpenEverest UI.
	ready := "false"
	if info.ProvisioningStatus == "ACTIVE" {
		ready = "true"
	}
	if err := r.updateWithRetry(ctx, client.ObjectKeyFromObject(lbc), func(latest *unstructured.Unstructured) error {
		anns := latest.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		anns[elbStatusAnnotation] = info.ProvisioningStatus
		anns[readyAnnotation] = ready
		anns[errorAnnotation] = ""
		if info.PublicIP != "" {
			anns[publicIPAnnotation] = info.PublicIP
		}
		latest.SetAnnotations(anns)
		return nil
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating ELB status annotations: %w", err)
	}

	if info.ProvisioningStatus == "ACTIVE" {
		return ctrl.Result{RequeueAfter: healthyRequeue}, nil
	}
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

	elbID := getSpecAnnotation(lbc, huaweicloud.AnnotationELBID)
	if elbID != "" {
		logger.Info("Deleting Huawei Cloud ELB", "elbID", elbID)
		if err := huaweicloud.DeleteELB(elbClient, elbID); err != nil {
			// If the ELB is already gone, proceed with finalizer removal.
			if !huaweicloud.IsNotFoundError(err) {
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

// isControlled returns true if the CR has huawei-elb.io/vpc-id in either
// spec.annotations (user-specified) or metadata.annotations (auto-detected),
// indicating it should be managed by this controller.
func isControlled(lbc *unstructured.Unstructured) bool {
	if getSpecAnnotation(lbc, vpcIDAnnotation) != "" {
		return true
	}
	anns := lbc.GetAnnotations()
	return anns[vpcIDAnnotation] != ""
}

// hasForeignCloudAnnotations returns true if the LBC's spec.annotations
// contains keys that target a different cloud provider (AWS, GKE, Azure,
// Alibaba). This prevents the controller from creating Huawei Cloud ELBs
// for LBCs meant for other clouds (e.g. OpenEverest's built-in eks-default).
func hasForeignCloudAnnotations(lbc *unstructured.Unstructured) bool {
	specAnns, found, _ := unstructured.NestedStringMap(lbc.Object, "spec", "annotations")
	if !found || specAnns == nil {
		return false
	}
	for key := range specAnns {
		for _, prefix := range foreignCloudAnnotationPrefixes {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
	}
	return false
}

// isInUse returns true if the CR has status.inUse == true,
// indicating it is referenced by a DatabaseCluster.
func isInUse(lbc *unstructured.Unstructured) bool {
	inUse, found, _ := unstructured.NestedBool(lbc.Object, "status", "inUse")
	return found && inUse
}

// autoDetectParams detects VPC ID, Neutron subnet ID, and availability zones
// from the Kubernetes cluster's nodes.
//
// The VPC ID is looked up via the ECS API using the first node's
// status.nodeInfo.machineID (the CCE-provisioned ECS server-id) — this avoids
// the CIDR-overlap false positives of the previous "list-all-VPCs-and-match"
// approach. The Neutron subnet ID is read from the
// "node.kubernetes.io/subnetid" label, which the CCE Cloud Controller Manager
// writes to every node. Availability zones come from the
// "topology.kubernetes.io/zone" label on all nodes.
//
// If the caller has manually specified huawei-elb.io/vpc-id in
// spec.annotations, that takes precedence and auto-detection is bypassed (no
// cache write, no API call) — so changing the override on a live cluster
// takes effect on the next reconcile without stale cached data.
//
// Results are cached since all nodes in a CCE cluster share the same VPC.
func (r *LoadBalancerConfigReconciler) autoDetectParams(
	ctx context.Context, logger logr.Logger, lbc *unstructured.Unstructured,
) (vpcID, subnetID string, azs []string, err error) {
	r.detectMu.Lock()
	defer r.detectMu.Unlock()

	// 0. If the user manually specified vpc-id, use those values directly and
	// skip both auto-detection and the cache. This makes manual overrides
	// hot-reloadable: editing spec.annotations on a live LBC picks up the new
	// values on the next reconcile without waiting for stale cached data.
	if manualVPC := getSpecAnnotation(lbc, vpcIDAnnotation); manualVPC != "" {
		manualSubnet := getSpecAnnotation(lbc, subnetIDAnnotation)
		manualAZs := strings.Split(getSpecAnnotation(lbc, availabilityZonesAnnotation), ",")
		logger.Info("Using manually specified VPC params; skipping auto-detection",
			"vpc-id", manualVPC, "subnet-id", manualSubnet, "azs", manualAZs)
		return manualVPC, manualSubnet, manualAZs, nil
	}

	// 1. Return cached result if available.
	if r.detected != nil {
		return r.detected.VPCID, r.detected.SubnetID, r.detected.AZs, nil
	}

	// 2. List all nodes.
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return "", "", nil, fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodeList.Items) == 0 {
		return "", "", nil, fmt.Errorf("no nodes found in cluster")
	}

	// 3. Take the first node — all CCE nodes share the same VPC.
	node := nodeList.Items[0]

	// 4. Get the Virsubnet ID from the node label written by CCE CCM.
	//    This is the VPC service's subnet ID; ELB needs the Neutron subnet ID,
	//    so we convert it below via the VPC API.
	virsubnetID := node.Labels["node.kubernetes.io/subnetid"]
	if virsubnetID == "" {
		return "", "", nil, fmt.Errorf(
			"node %s has no node.kubernetes.io/subnetid label", node.Name)
	}

	// 4b. Convert the Virsubnet ID to a Neutron subnet ID for the ELB API.
	subnetID, err = huaweicloud.GetNeutronSubnetID(r.Creds, virsubnetID)
	if err != nil {
		return "", "", nil, fmt.Errorf("converting virsubnet to neutron subnet: %w", err)
	}

	// 5. Collect availability zones from every node's zone label.
	azSet := make(map[string]bool)
	for _, n := range nodeList.Items {
		if az, ok := n.Labels["topology.kubernetes.io/zone"]; ok {
			azSet[az] = true
		}
	}
	for az := range azSet {
		azs = append(azs, az)
	}
	sort.Strings(azs)
	if len(azs) == 0 {
		return "", "", nil, fmt.Errorf(
			"no availability zones found in node labels (topology.kubernetes.io/zone)",
		)
	}

	// 6. Get VPC ID from the ECS API via the node's machineID (ECS server-id).
	// This is the most reliable signal: every CCE node carries the cluster's
	// VPC ID in its server metadata, with no CIDR overlap ambiguity.
	serverID := node.Status.NodeInfo.MachineID
	if serverID == "" {
		return "", "", nil, fmt.Errorf("node %s has no machineID", node.Name)
	}
	vpcID, err = huaweicloud.DetectVPCFromECS(r.Creds, serverID)
	if err != nil {
		return "", "", nil, err
	}

	// 7. Cache the result.
	r.detected = &autoDetectedParams{
		VPCID:    vpcID,
		SubnetID: subnetID,
		AZs:      azs,
	}

	logger.Info("Auto-detected cluster network params from nodes",
		"vpc-id", vpcID, "subnet-id", subnetID,
		"azs", azs, "node-count", len(nodeList.Items))

	return vpcID, subnetID, azs, nil
}

// shouldReconcile returns true if the LBC should be processed by this controller.
// It skips LBCs that use CCM's autocreate annotation (kubernetes.io/elb.autocreate),
// as those are managed by CCE's CCM directly.
func shouldReconcile(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return false
	}
	// Skip LBCs using CCM autocreate.
	if getSpecAnnotation(u, ccmAutocreateAnnotation) != "" {
		return false
	}
	// Process everything else, including empty LBCs (for auto-detection).
	return true
}

// getSpecAnnotations returns all annotations from spec.annotations as a map.
func getSpecAnnotations(lbc *unstructured.Unstructured) map[string]string {
	anns, found, _ := unstructured.NestedStringMap(lbc.Object, "spec", "annotations")
	if !found {
		return map[string]string{}
	}
	return anns
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

// parseELBOptions reads ELB creation parameters.
//
// Required params are read from spec.annotations (user-specified) first,
// falling back to metadata.annotations (auto-detected). This avoids writing
// auto-detected params to spec.annotations, which would conflict with the
// OpenEverest UI.
//
// Required params (spec.annotations or metadata.annotations):
//   - huawei-elb.io/vpc-id
//   - huawei-elb.io/subnet-id
//   - huawei-elb.io/availability-zones (comma-separated)
//
// Optional params (spec.annotations only, for public ELB):
//   - huawei-elb.io/public: "false" for internal ELB (default "true", public)
//   - huawei-elb.io/bandwidth-size: e.g. "20" (default 10)
//   - huawei-elb.io/bandwidth-charge-mode: "traffic" or "bandwidth" (default "traffic")
//   - huawei-elb.io/public-ip-network-type: e.g. "5_bgp" (default "5_bgp")
//
// Note: kubernetes.io/elb.id is written to spec.annotations after ELB creation
// so the OpenEverest operator can copy it to the K8s LoadBalancer Service.
func parseELBOptions(lbc *unstructured.Unstructured) (*huaweicloud.CreateELBOption, error) {
	specAnns := getSpecAnnotations(lbc)
	metaAnns := lbc.GetAnnotations()
	if metaAnns == nil {
		metaAnns = map[string]string{}
	}

	// Prefer spec.annotations (user-specified), fall back to metadata.annotations (auto-detected).
	vpcID := specAnns[vpcIDAnnotation]
	if vpcID == "" {
		vpcID = metaAnns[vpcIDAnnotation]
	}
	subnetID := specAnns[subnetIDAnnotation]
	if subnetID == "" {
		subnetID = metaAnns[subnetIDAnnotation]
	}
	azStr := specAnns[availabilityZonesAnnotation]
	if azStr == "" {
		azStr = metaAnns[availabilityZonesAnnotation]
	}

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

	// Default to public ELB. Set huawei-elb.io/public: "false" for internal.
	opt.IsPublic = true
	if strings.EqualFold(specAnns[publicAnnotation], "false") {
		opt.IsPublic = false
	}
	if opt.IsPublic {
		if bw, err := strconv.Atoi(specAnns[bandwidthSizeAnnotation]); err == nil && bw > 0 {
			opt.BandwidthSize = int32(bw)
		}
		opt.BandwidthChargeMode = specAnns[bandwidthChargeModeAnnotation]
		opt.PublicIPNetworkType = specAnns[publicIPNetworkTypeAnnotation]
	}

	return opt, nil
}

// SetupWithManager registers the controller with the manager.
// The predicate skips LBCs that use CCM autocreate (kubernetes.io/elb.autocreate),
// as those are managed by CCE's CCM directly. All other LBCs are processed,
// including empty ones — auto-detection fills in VPC/subnet/AZ from cluster nodes.
func (r *LoadBalancerConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	lbc := &unstructured.Unstructured{}
	lbc.SetGroupVersionKind(lbcGVR)

	controlledPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return shouldReconcile(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return shouldReconcile(e.ObjectOld) || shouldReconcile(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			// Always process deletes to clean up finalizers.
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return shouldReconcile(e.Object)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(lbc).
		WithEventFilter(controlledPredicate).
		Complete(r)
}
