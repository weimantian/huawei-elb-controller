package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/weimantian/huawei-elb-controller/api/v1alpha1"
	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

type ServiceReconciler struct {
	client.Client
	ELBClient       *elb.ElbClient
	NetworkDetector *huaweicloud.NetworkDetector
	Creds           *huaweicloud.Credentials
	Scheme          *runtime.Scheme
}

// patchWithRetry applies a patch with conflict retry. The modifyFn is called
// on the latest object version each attempt.
func (r *ServiceReconciler) patchWithRetry(
	ctx context.Context, key types.NamespacedName, modifyFn func(*corev1.Service) error,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1.Service{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		original := latest.DeepCopy()
		if err := modifyFn(latest); err != nil {
			return err
		}
		return r.Patch(ctx, latest, client.MergeFrom(original))
	})
}

// updateStatusWithRetry updates service.status.loadBalancer.ingress with conflict retry.
func (r *ServiceReconciler) updateStatusWithRetry(
	ctx context.Context, key types.NamespacedName, ingress []corev1.LoadBalancerIngress,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1.Service{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		original := latest.DeepCopy()
		latest.Status.LoadBalancer.Ingress = ingress
		return r.Status().Patch(ctx, latest, client.MergeFrom(original))
	})
}

const (
	lastKnownParamsAnnotation = "huawei-elb.io/last-known-params"
	serviceRequeue            = 5 * time.Minute
	serviceRetryRequeue       = 10 * time.Second
	elbActiveWaitRequeue      = 5 * time.Second
	elbProvisionTimeout       = 2 * time.Minute

	aclCleanupFinalizer = "huawei-elb.io/acl-cleanup"

	// ACL annotations in our own namespace (not kubernetes.io/elb.*)
	aclIDAnnotation     = "huawei-elb.io/acl-id"
	aclStatusAnnotation = "huawei-elb.io/acl-status"
	aclTypeAnnotation   = "huawei-elb.io/acl-type"
	sourceRangesKey     = "source-ranges"

	tcpProtocol    = "TCP"
	lbAlgorithmRR  = "ROUND_ROBIN"
)

// getBinding returns the ELBBinding for the given Service, or nil if it does not exist.
func (r *ServiceReconciler) getBinding(ctx context.Context, svc *corev1.Service) (*v1alpha1.ELBBinding, error) {
	binding := &v1alpha1.ELBBinding{}
	key := types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}
	if err := r.Get(ctx, key, binding); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return binding, nil
}

// ensureBinding returns an ELBBinding for the Service, creating one if necessary.
// If an existing binding has a mismatched ServiceUID (stale from name reuse), it is deleted first.
func (r *ServiceReconciler) ensureBinding(ctx context.Context, svc *corev1.Service) (*v1alpha1.ELBBinding, error) {
	binding, err := r.getBinding(ctx, svc)
	if err != nil {
		return nil, err
	}
	if binding != nil {
		if binding.Spec.ServiceUID != string(svc.UID) {
			if err := r.Delete(ctx, binding); err != nil {
				return nil, err
			}
			binding = nil
		}
	}
	if binding == nil {
		binding = &v1alpha1.ELBBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svc.Name,
				Namespace: svc.Namespace,
			},
			Spec: v1alpha1.ELBBindingSpec{
				ServiceName: svc.Name,
				ServiceUID:  string(svc.UID),
			},
		}
		if r.Scheme != nil {
			_ = controllerutil.SetOwnerReference(svc, binding, r.Scheme)
		}
		if err := r.Create(ctx, binding); err != nil {
			return nil, err
		}
	}
	return binding, nil
}

// patchBindingStatus patches the status subresource of an ELBBinding with conflict retry.
func (r *ServiceReconciler) patchBindingStatus(
	ctx context.Context, key types.NamespacedName, mutate func(*v1alpha1.ELBBinding) error,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.ELBBinding{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		original := latest.DeepCopy()
		if err := mutate(latest); err != nil {
			return err
		}
		return r.Status().Patch(ctx, latest, client.MergeFrom(original))
	})
}

// adoptLegacyAnnotations populates the ELBBinding status from legacy Service annotations
// for zero-downtime migration of pre-existing ELB Services.
// After adoption, strips legacy annotations from the Service so state is stored
// exclusively in the ELBBinding CRD.
func (r *ServiceReconciler) adoptLegacyAnnotations(ctx context.Context, logger logr.Logger, key types.NamespacedName, svc *corev1.Service) error {
	var annotationsToStrip []string
	if err := r.patchBindingStatus(ctx, key, func(b *v1alpha1.ELBBinding) error {
		if elbID := svc.Annotations[huaweicloud.AnnotationELBID]; elbID != "" {
			b.Status.ELBID = elbID
			annotationsToStrip = append(annotationsToStrip, huaweicloud.AnnotationELBID)
		}
		if aclID := svc.Annotations[aclIDAnnotation]; aclID != "" {
			b.Status.ACLID = aclID
			annotationsToStrip = append(annotationsToStrip, aclIDAnnotation)
		}
		if aclStatus := svc.Annotations[aclStatusAnnotation]; aclStatus != "" {
			b.Status.ACLStatus = aclStatus
			annotationsToStrip = append(annotationsToStrip, aclStatusAnnotation)
		}
		if aclType := svc.Annotations[aclTypeAnnotation]; aclType != "" {
			b.Status.ACLType = aclType
			annotationsToStrip = append(annotationsToStrip, aclTypeAnnotation)
		}
		if lastKnownJSON := svc.Annotations[lastKnownParamsAnnotation]; lastKnownJSON != "" {
			lastKnownParams := make(map[string]string)
			_ = json.Unmarshal([]byte(lastKnownJSON), &lastKnownParams)
			b.Status.LastKnownParams = lastKnownParams
			annotationsToStrip = append(annotationsToStrip, lastKnownParamsAnnotation)
		}
		if len(svc.Status.LoadBalancer.Ingress) > 0 {
			b.Status.IngressIP = svc.Status.LoadBalancer.Ingress[0].IP
		}
		if b.Status.Phase == "" {
			b.Status.Phase = v1alpha1.PhaseReady
		}
		return nil
	}); err != nil {
		return err
	}
	// Strip legacy annotations from Service after successful migration.
	if len(annotationsToStrip) > 0 {
		if err := r.patchWithRetry(ctx, key, func(latest *corev1.Service) error {
			if latest.Annotations == nil {
				return nil
			}
			for _, ann := range annotationsToStrip {
				delete(latest.Annotations, ann)
			}
			return nil
		}); err != nil {
			logger.Error(err, "stripping legacy annotations after ELBBinding adoption")
			// Non-fatal: annotations will be cleaned on the next update reconcile.
		} else {
			logger.Info("stripped legacy annotations from Service after ELBBinding migration")
		}
	}
	return nil
}
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !isLoadBalancerService(svc) {
		return ctrl.Result{}, nil
	}

	// Deletion cleanup takes priority.
	if !svc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, svc)
	}

	// Skip legacy CCM-managed Services.
	if hasLegacyELBID(svc) || hasLegacyAutocreate(svc) {
		return ctrl.Result{}, nil
	}

	if hasForeignCloudServiceAnnotations(svc) {
		return ctrl.Result{}, nil
	}

	if !isOpenEverestService(svc) {
		return ctrl.Result{}, nil
	}

	logger.Info("Reconciling Service", "name", svc.Name, "namespace", svc.Namespace)

	if hasManagedELBID(svc) {
		return r.reconcileUpdate(ctx, logger, svc)
	}
	return r.reconcileCreate(ctx, logger, svc)
}

// reconcileCreate creates a new ELB with listener/pool/members/healthcheck
// via direct Huawei Cloud API (Plan B - no CCM autocreate annotations).
func (r *ServiceReconciler) reconcileCreate(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, error) {
	lbcParams := getLBCParams(svc)

	vpcID, subnetID, azs, err := r.NetworkDetector.Detect(ctx, r.Client)
	if err != nil {
		logger.Error(err, "network detection failed")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Guard against annotation loss: OpenEverest may overwrite our huawei-elb.io/elb-id
	// annotation when syncing LBC params, causing Reconcile to route back to create.
	// Before creating, check if an ELB with the expected name already exists.
	// If found, restore the annotation + finalizer and return; the next reconcile
	// will route to reconcileUpdate and complete provisioning.
	//
	// All ELB names include a UID suffix (both default and custom-named), so the
	// name is globally unique -- a match always means THIS Service lost its annotation.
	elbName := huaweicloud.BuildELBName(lbcParams, svc.Namespace, svc.Name, string(svc.UID))
	existing, findErr := huaweicloud.FindELBByName(r.ELBClient, elbName)
	if findErr != nil {
		logger.Error(findErr, "checking for existing ELB by name before create")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
	if existing != nil {
		logger.Info("Found existing ELB by name, restoring ELBBinding and routing to update", "elbID", existing.ID, "name", elbName)
		// Write ELB ID to ELBBinding status (not Service annotation).
		bindingKey := types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}
		if _, err := r.ensureBinding(ctx, svc); err != nil {
			logger.Error(err, "ensuring ELBBinding during reverse-lookup")
		}
		_ = r.patchBindingStatus(ctx, bindingKey, func(b *v1alpha1.ELBBinding) error {
			b.Status.ELBID = existing.ID
			return nil
		})
		// Still need finalizer on Service (actual cleanup trigger).
		if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
			controllerutil.AddFinalizer(latest, huaweicloud.AnnotationELBCleanupFinalizer)
			return nil
		}); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Info("Service gone during reverse-lookup, deleting orphaned ELB", "elbID", existing.ID, "name", elbName)
				if delErr := r.deleteELBStack(logger, existing.ID); delErr != nil && !huaweicloud.IsNotFoundError(delErr) {
					logger.Error(delErr, "deleting orphaned ELB after Service disappeared", "elbID", existing.ID)
				}
				return ctrl.Result{}, nil
			}
			logger.Error(err, "adding cleanup finalizer during reverse-lookup")
			return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
		}
		return ctrl.Result{Requeue: true}, nil
	}
	// Create the ELB.
	opt := huaweicloud.BuildCreateELBOption(lbcParams, vpcID, subnetID, azs, svc.Namespace, svc.Name, string(svc.UID))
	logger.Info("Creating ELB via direct API", "name", opt.Name, "public", opt.IsPublic)
	info, err := huaweicloud.CreateELB(r.ELBClient, opt)
	if err != nil {
		logger.Error(err, "creating ELB")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
	logger.Info("ELB created", "elbID", info.ID, "status", info.ProvisioningStatus)

	// Wait for ELB to become ACTIVE before creating child resources.
	if info.ProvisioningStatus != "ACTIVE" {
		active, err := huaweicloud.WaitForELBActive(r.ELBClient, info.ID, elbProvisionTimeout)
		if err != nil {
			logger.Error(err, "waiting for ELB active", "elbID", info.ID)
			return ctrl.Result{RequeueAfter: elbActiveWaitRequeue}, nil
		}
		if !active {
			logger.Info("ELB not yet active, will retry", "elbID", info.ID)
			return ctrl.Result{RequeueAfter: elbActiveWaitRequeue}, nil
		}
	}
	// CRITICAL: Ensure ELBBinding exists immediately after ELB creation.
	// This ensures that if subsequent steps (listener/pool/ACL/status) fail or the
	// process crashes, the next reconcile will find the binding and route to
	// reconcileUpdate, which can complete provisioning via syncListenerStacks.
	// Without this, a crash after ELB creation but before binding write would
	// orphan the ELB (leak) and create a duplicate on retry.
	if _, err := r.ensureBinding(ctx, svc); err != nil {
		logger.Error(err, "ensuring ELBBinding")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	bindingKey := types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}
	compositeParams := buildCompositeParams(lbcParams, filterValidCIDRs(logger, svc.Spec.LoadBalancerSourceRanges))

	if err := r.patchBindingStatus(ctx, bindingKey, func(b *v1alpha1.ELBBinding) error {
		b.Status.ELBID = info.ID
		b.Status.LastKnownParams = compositeParams
		b.Status.Phase = v1alpha1.PhaseProvisioning
		return nil
	}); err != nil {
		logger.Error(err, "patching ELBBinding status with ELB ID")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Persist cleanup finalizer on the Service (must stay on Service to
	// ensure cleanup fires even if the controller is down during deletion).
	// Persist cleanup finalizer on the Service (must stay on Service to
	// ensure cleanup fires even if the controller is down during deletion).
	// State (elbID, params) is stored ONLY in ELBBinding status.
	if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
		controllerutil.AddFinalizer(latest, huaweicloud.AnnotationELBCleanupFinalizer)
		return nil
	}); err != nil {
		logger.Error(err, "patching Service finalizer")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Get node backends for member creation (NodePort mode, multi-AZ aware).
	backends, err := r.getNodeBackends(ctx, svc)
	if err != nil {
		logger.Error(err, "getting node backends")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Create listener + pool + healthcheck + members for each Service port.
	// If this fails mid-way, reconcileUpdate's syncListenerStacks will complete
	// the remaining ports on the next reconcile.
	for _, port := range svc.Spec.Ports {
		if err := r.createListenerStack(ctx, logger, info.ID, svc, port, backends); err != nil {
			logger.Error(err, "creating listener stack, will complete on retry", "port", port.Port)
			return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
		}
	}

	// ACL handling (IP groups for source ranges).
	aclID, aclStatus, aclType, aclFinalizer, err := r.ensureACL(ctx, logger, info.ID, "" /* no existing ACL on create */, svc)
	if err != nil {
		logger.Error(err, "ensuring ACL, will retry")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
	// Persist ACL state to ELBBinding status.
	if err := r.patchBindingStatus(ctx, bindingKey, func(b *v1alpha1.ELBBinding) error {
		b.Status.ACLID = aclID
		b.Status.ACLStatus = aclStatus
		b.Status.ACLType = aclType
		return nil
	}); err != nil {
		logger.Error(err, "patching ELBBinding ACL status")
	}
	// Manage ACL finalizer on Service.
	if aclFinalizer {
		if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
			controllerutil.AddFinalizer(latest, aclCleanupFinalizer)
			return nil
		}); err != nil {
			logger.Error(err, "adding ACL finalizer")
			return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
		}
	}

	// Write service.status.loadBalancer.ingress.
	ingressIP := info.VipAddress
	if info.PublicIP != "" {
		ingressIP = info.PublicIP
	}
	if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(svc), []corev1.LoadBalancerIngress{
		{IP: ingressIP},
	}); err != nil {
		logger.Error(err, "updating Service status")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Persist ingress IP and final Ready phase to ELBBinding status.
	if err := r.patchBindingStatus(ctx, bindingKey, func(b *v1alpha1.ELBBinding) error {
		b.Status.IngressIP = ingressIP
		b.Status.Phase = v1alpha1.PhaseReady
		return nil
	}); err != nil {
		logger.Error(err, "patching ELBBinding final status")
	}

	logger.Info("ELB fully provisioned", "elbID", info.ID, "ingressIP", ingressIP)
	return ctrl.Result{RequeueAfter: serviceRequeue}, nil
}

// createListenerStack creates a listener, pool, healthcheck, and members for one port.
func (r *ServiceReconciler) createListenerStack(
	ctx context.Context, logger logr.Logger, elbID string,
	svc *corev1.Service, port corev1.ServicePort, backends []NodeBackend,
) error {
	listenerName := fmt.Sprintf("%s-%d", svc.Name, port.Port)
	listener, err := huaweicloud.CreateListener(r.ELBClient, elbID, listenerName, port.Port, tcpProtocol)
	if err != nil {
		return fmt.Errorf("creating listener: %w", err)
	}

	poolName := fmt.Sprintf("pool-%s-%d", svc.Name, port.Port)
	poolID, err := huaweicloud.CreatePool(r.ELBClient, listener.ID, poolName, tcpProtocol, lbAlgorithmRR)
	if err != nil {
		return fmt.Errorf("creating pool: %w", err)
	}

	hcCfg := huaweicloud.DefaultHealthCheckConfig()
	if _, err := huaweicloud.CreateHealthCheck(r.ELBClient, poolID, hcCfg); err != nil {
		return fmt.Errorf("creating health check: %w", err)
	}

	// Add members (node IP + NodePort), each with its own subnet (multi-AZ support).
	if port.NodePort > 0 {
		for _, be := range backends {
			subnetID, err := r.NetworkDetector.GetNeutronSubnet(be.VirsubnetID)
			if err != nil {
				return fmt.Errorf("resolving subnet for node %s: %w", be.IP, err)
			}
			if _, err := huaweicloud.AddMember(r.ELBClient, poolID, be.IP, port.NodePort, subnetID); err != nil {
				return fmt.Errorf("adding member %s:%d: %w", be.IP, port.NodePort, err)
			}
		}
	}

	logger.Info("Listener stack created", "port", port.Port, "listener", listener.ID, "pool", poolID)
	return nil
}

// reconcileUpdate handles changes to an existing ELB: port changes, node changes,
// param changes (bandwidth), and ACL changes.
func (r *ServiceReconciler) reconcileUpdate(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, error) {
	// Prefer ELBBinding for ELB ID, falling back to annotation for backward compat.
	elbID := ""
	bindingKey := types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}
	binding, err := r.getBinding(ctx, svc)
	if err != nil {
		logger.Error(err, "getting ELBBinding")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
	if binding != nil && binding.Status.ELBID != "" {
		elbID = binding.Status.ELBID
	}
	if elbID == "" {
		elbID = svc.Annotations[huaweicloud.AnnotationELBID]
	}
	if elbID == "" {
		logger.Info("ELB ID missing, falling back to create")
		return r.reconcileCreate(ctx, logger, svc)
	}

	// Ensure ELBBinding exists for future use. Adopt legacy annotations if needed.
	if binding == nil {
		created, err := r.ensureBinding(ctx, svc)
		if err != nil {
			logger.Error(err, "ensuring ELBBinding")
		} else if created != nil {
			// Adopt legacy annotation values into the new ELBBinding.
			binding = created
			if err := r.adoptLegacyAnnotations(ctx, logger, bindingKey, svc); err != nil {
				logger.Error(err, "adopting legacy annotations to ELBBinding")
			} else {
				logger.Info("adopted legacy annotations into ELBBinding", "elbID", elbID)
			}
		}
	}

	// 1. Sync listener/pool stack for port changes.
	if err := r.syncListenerStacks(ctx, logger, elbID, svc); err != nil {
		logger.Error(err, "syncing listener stacks")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// 2. Sync pool members for node changes.
	if err := r.syncAllPoolMembers(ctx, logger, elbID, svc); err != nil {
		logger.Error(err, "syncing pool members")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// 3. Handle LBC param changes (bandwidth etc.).
	currentParams := getLBCParams(svc)
	lastKnownParams := make(map[string]string)
	if binding != nil && len(binding.Status.LastKnownParams) > 0 {
		for k, v := range binding.Status.LastKnownParams {
			lastKnownParams[k] = v
		}
	} else {
		// Fallback to annotation
		lastKnownJSON := svc.Annotations[lastKnownParamsAnnotation]
		if lastKnownJSON != "" {
			_ = json.Unmarshal([]byte(lastKnownJSON), &lastKnownParams)
		}
	}
	delete(lastKnownParams, sourceRangesKey)

	if !paramsEqual(currentParams, lastKnownParams) {
		opt := buildUpdateOption(currentParams, lastKnownParams)
		if opt.BandwidthSize > 0 || opt.BandwidthChargeMode != "" {
			if err := huaweicloud.UpdateELB(r.ELBClient, elbID, opt, r.Creds); err != nil {
				logger.Error(err, "updating ELB params", "elbID", elbID)
				return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
			}
			logger.Info("ELB params updated", "elbID", elbID, "bandwidthSize", opt.BandwidthSize, "chargeMode", opt.BandwidthChargeMode)
		}
	}

	// 4. Handle ACL changes (source ranges).
	existingACLID := ""
	if binding != nil {
		existingACLID = binding.Status.ACLID
	}
	// Fallback: after legacy adoption, the in-memory binding may not reflect
	// the API server state yet, so check Service annotations as well.
	if existingACLID == "" {
		existingACLID = svc.Annotations[aclIDAnnotation]
	}
	aclID, aclStatus, aclType, aclFinalizer, err := r.ensureACL(ctx, logger, elbID, existingACLID, svc)
	if err != nil {
		logger.Error(err, "ensuring ACL")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// 5. Persist last-known-params and ACL state to ELBBinding status.
	compositeParams := buildCompositeParams(currentParams, filterValidCIDRs(logger, svc.Spec.LoadBalancerSourceRanges))

	if err := r.patchBindingStatus(ctx, bindingKey, func(b *v1alpha1.ELBBinding) error {
		b.Status.LastKnownParams = compositeParams
		b.Status.ACLID = aclID
		b.Status.ACLStatus = aclStatus
		b.Status.ACLType = aclType
		return nil
	}); err != nil {
		logger.Error(err, "patching ELBBinding status")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
	// Manage ACL finalizer on Service.
	_ = r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
		if aclFinalizer {
			controllerutil.AddFinalizer(latest, aclCleanupFinalizer)
		} else {
			controllerutil.RemoveFinalizer(latest, aclCleanupFinalizer)
		}
		return nil
	})

	// Ensure status has the ELB IP (fixes status lost during create-time RBAC failure).
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		info, err := huaweicloud.ShowELB(r.ELBClient, elbID)
		if err != nil {
			logger.Error(err, "getting ELB for status update", "elbID", elbID)
		} else {
			ingressIP := info.VipAddress
			if info.PublicIP != "" {
				ingressIP = info.PublicIP
			}
			if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(svc), []corev1.LoadBalancerIngress{{IP: ingressIP}}); err != nil {
				logger.Error(err, "updating Service status", "elbID", elbID)
			}

			// Also persist ingress IP to ELBBinding.
			_ = r.patchBindingStatus(ctx, bindingKey, func(b *v1alpha1.ELBBinding) error {
				b.Status.IngressIP = ingressIP
				return nil
			})
		}
	}

	return ctrl.Result{RequeueAfter: serviceRequeue}, nil
}

// syncListenerStacks diffs Service ports against existing listeners and adds/removes.
func (r *ServiceReconciler) syncListenerStacks(ctx context.Context, logger logr.Logger, elbID string, svc *corev1.Service) error {
	listeners, err := huaweicloud.ListListeners(r.ELBClient, elbID)
	if err != nil {
		return fmt.Errorf("listing listeners: %w", err)
	}

	// Build maps for diffing.
	desiredPorts := make(map[int32]corev1.ServicePort, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		desiredPorts[p.Port] = p
	}
	existingPorts := make(map[int32]huaweicloud.ListenerInfo, len(listeners))
	for _, l := range listeners {
		existingPorts[l.ProtocolPort] = l
	}

	backends, err := r.getNodeBackends(ctx, svc)
	if err != nil {
		// Skip listener sync if we can't list nodes; avoid creating listeners with no members.
		logger.Info("Skipping listener sync: failed to list nodes", "error", err)
		return nil
	}
	
	// Add new ports.
	for port, svcPort := range desiredPorts {
		if _, exists := existingPorts[port]; !exists {
			logger.Info("Adding listener for new port", "port", port)
			if err := r.createListenerStack(ctx, logger, elbID, svc, svcPort, backends); err != nil {
				return fmt.Errorf("creating listener for port %d: %w", port, err)
			}
		}
	}

	// Remove deleted ports.
	for port, listener := range existingPorts {
		if _, exists := desiredPorts[port]; !exists {
			logger.Info("Removing listener for deleted port", "port", port, "listener", listener.ID)
			if err := huaweicloud.DeleteListener(r.ELBClient, listener.ID); err != nil {
				return fmt.Errorf("deleting listener for port %d: %w", port, err)
			}
		}
	}

	return nil
}

// syncAllPoolMembers syncs members for all pools on the ELB based on current nodes.
func (r *ServiceReconciler) syncAllPoolMembers(ctx context.Context, logger logr.Logger, elbID string, svc *corev1.Service) error {
	pools, err := huaweicloud.ListPools(r.ELBClient, elbID)
	if err != nil {
		return fmt.Errorf("listing pools: %w", err)
	}

	backends, err := r.getNodeBackends(ctx, svc)
	if err != nil {
		// CRITICAL: Skip member sync on API failure to avoid clearing all members
		// with an empty list. Transient API failures must not cause service disruption.
		logger.Info("Skipping pool member sync: failed to list nodes", "error", err)
		return nil
	}
	
	for _, pool := range pools {
		// Match pool to Service port by name (pool-<svc-name>-<port>).
		var nodePort int32
		for _, p := range svc.Spec.Ports {
			if pool.Name == fmt.Sprintf("pool-%s-%d", svc.Name, p.Port) {
				nodePort = p.NodePort
				break
			}
		}
		if nodePort == 0 {
			logger.Info("Could not determine NodePort for pool, skipping", "pool", pool.ID, "poolName", pool.Name)
			continue
		}

		desired := make([]huaweicloud.MemberTarget, 0, len(backends))
		for _, be := range backends {
			subnetID, err := r.NetworkDetector.GetNeutronSubnet(be.VirsubnetID)
			if err != nil {
				return fmt.Errorf("resolving subnet for node %s: %w", be.IP, err)
			}
			desired = append(desired, huaweicloud.MemberTarget{
				Address:      be.IP,
				ProtocolPort: nodePort,
				SubnetID:     subnetID,
			})
		}

		if err := huaweicloud.SyncMembers(r.ELBClient, pool.ID, desired); err != nil {
			return fmt.Errorf("syncing members for pool %s: %w", pool.ID, err)
		}
		logger.Info("Pool members synced", "pool", pool.ID, "memberCount", len(desired), "externalTrafficPolicy", svc.Spec.ExternalTrafficPolicy)
	}

	return nil
}

// reconcileDelete deletes the ELB and ACL IP group, then removes finalizers.
func (r *ServiceReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, error) {
	// Resolve ELB ID from ELBBinding first, falling back to annotation.
	var elbID string
	bindingKey := types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}
	binding, err := r.getBinding(ctx, svc)
	if err != nil {
		logger.Error(err, "getting ELBBinding for deletion")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
	if binding != nil && binding.Status.ELBID != "" {
		elbID = binding.Status.ELBID
	}
	if elbID == "" {
		elbID = svc.Annotations[huaweicloud.AnnotationELBID]
	}

	// Delete ELB if we own it.
	if controllerutil.ContainsFinalizer(svc, huaweicloud.AnnotationELBCleanupFinalizer) {
		if elbID != "" {
			if err := r.deleteELBStack(logger, elbID); err != nil {
				if !huaweicloud.IsNotFoundError(err) {
					logger.Error(err, "deleting ELB, will retry", "elbID", elbID)
					return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
				}
				logger.Info("ELB already deleted", "elbID", elbID)
			}
		}
		// Clear service status.
		_ = r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(svc), nil)
		// Remove ELB cleanup finalizer.
		if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
			controllerutil.RemoveFinalizer(latest, huaweicloud.AnnotationELBCleanupFinalizer)
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Delete ACL IP group if present.
	if controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer) {
		ipGroupID := ""
		if binding != nil {
			ipGroupID = binding.Status.ACLID
		}
		if ipGroupID == "" {
			ipGroupID = svc.Annotations[aclIDAnnotation]
		}
		if ipGroupID == "" {
			// Annotation was overwritten (e.g. by OpenEverest LBC template sync or PSMDB
			// operator Update). Fall back to name-based lookup so the IP group doesn't leak.
			ipGroupName := "acl-" + svc.Namespace + "-" + svc.Name
			foundID, findErr := huaweicloud.FindIPGroupByName(r.ELBClient, ipGroupName)
			if findErr != nil {
				logger.Error(findErr, "looking up ACL IP group by name (annotation was lost)", "name", ipGroupName)
				return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
			}
			ipGroupID = foundID
		}
		if ipGroupID != "" {
			if err := huaweicloud.DeleteIPGroup(r.ELBClient, ipGroupID); err != nil {
				if !huaweicloud.IsNotFoundError(err) {
					logger.Error(err, "deleting ACL IP group, will retry", "ipGroupID", ipGroupID)
					return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
				}
			}
			logger.Info("ACL IP group deleted", "ipGroupID", ipGroupID)
		}
		if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
			controllerutil.RemoveFinalizer(latest, aclCleanupFinalizer)
			return nil
		}); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Delete the ELBBinding if it exists.
	if binding != nil {
		if err := r.Delete(ctx, binding); err != nil {
			logger.Error(err, "deleting ELBBinding", "binding", bindingKey)
			// Non-fatal: Service finalizers are already removed, binding will be
			// cleaned up on next reconcile (no matching Service -> no-op).
		} else {
			logger.Info("ELBBinding deleted", "binding", bindingKey)
		}
	}

	return ctrl.Result{}, nil
}

// deleteELBStack deletes all child resources on the ELB in dependency order, then deletes the ELB.
// Order: HealthCheck -> Pool (cascades Member) -> Listener -> ELB.
// Huawei Cloud API requires each child resource to be deleted before its parent.
func (r *ServiceReconciler) deleteELBStack(logger logr.Logger, elbID string) error {
	pools, err := huaweicloud.ListPools(r.ELBClient, elbID)
	if err != nil {
		return fmt.Errorf("listing pools for deletion: %w", err)
	}

	// Delete health checks, members, then pools for each pool.
	for _, p := range pools {
		// Delete health check for this pool.
		hcs, err := huaweicloud.ListHealthChecks(r.ELBClient)
		if err != nil {
			return fmt.Errorf("listing health checks: %w", err)
		}
		for _, hc := range hcs {
			if hc.PoolID == p.ID {
				logger.Info("Deleting health check", "healthcheck", hc.ID, "pool", p.ID)
				if err := huaweicloud.DeleteHealthCheck(r.ELBClient, hc.ID); err != nil {
					return fmt.Errorf("deleting health check %s: %w", hc.ID, err)
				}
			}
		}

		// Delete all members in this pool.
		members, err := huaweicloud.ListMembers(r.ELBClient, p.ID)
		if err != nil {
			return fmt.Errorf("listing members for pool %s: %w", p.ID, err)
		}
		for _, m := range members {
			logger.Info("Deleting member", "member", m.ID, "pool", p.ID)
			if err := huaweicloud.DeleteMember(r.ELBClient, p.ID, m.ID); err != nil {
				return fmt.Errorf("deleting member %s: %w", m.ID, err)
			}
		}

		// Now safe to delete the pool.
		logger.Info("Deleting pool", "pool", p.ID, "name", p.Name)
		if err := huaweicloud.DeletePool(r.ELBClient, p.ID); err != nil {
			return fmt.Errorf("deleting pool %s: %w", p.ID, err)
		}
	}

	listeners, err := huaweicloud.ListListeners(r.ELBClient, elbID)
	if err != nil {
		return fmt.Errorf("listing listeners for deletion: %w", err)
	}
	for _, l := range listeners {
		logger.Info("Deleting listener before ELB deletion", "listener", l.ID, "port", l.ProtocolPort)
		if err := huaweicloud.DeleteListener(r.ELBClient, l.ID); err != nil {
			return fmt.Errorf("deleting listener %s: %w", l.ID, err)
		}
	}
	// Get EIP ID before deleting ELB (ShowELB fails after ELB is deleted).
	eipID := ""
	info, showErr := huaweicloud.ShowELB(r.ELBClient, elbID)
	if showErr != nil {
		if !huaweicloud.IsNotFoundError(showErr) {
			logger.Error(showErr, "showing ELB to get EIP ID before deletion", "elbID", elbID)
		}
	} else {
		eipID = info.EipID
	}

	// Delete the ELB.
	if err := huaweicloud.DeleteELB(r.ELBClient, elbID); err != nil {
		return err
	}
	logger.Info("ELB deleted", "elbID", elbID)

	// Delete the EIP if the ELB had one.
	// DeleteELB unbinds the EIP but does not delete it; without this step the EIP leaks.
	if eipID != "" {
		logger.Info("Deleting EIP after ELB deletion", "eipID", eipID, "elbID", elbID)
		if err := huaweicloud.DeleteEIPByID(r.Creds, eipID); err != nil {
			logger.Error(err, "deleting EIP, manual cleanup needed", "eipID", eipID)
			// Don't fail: ELB is already deleted, EIP is just orphaned.
		} else {
			logger.Info("EIP deleted", "eipID", eipID, "elbID", elbID)
		}
	}

	return nil
}

// ensureACL creates/updates/deletes the ACL IP group based on loadBalancerSourceRanges.
// On success returns the ACL state values; the caller persists them to ELBBinding status.
// existingACLID is read from ELBBinding status (not Service annotations).
func (r *ServiceReconciler) ensureACL(ctx context.Context, logger logr.Logger, elbID, existingACLID string, svc *corev1.Service) (aclID, aclStatus, aclType string, wantFinalizer bool, err error) {
	sourceRanges := svc.Spec.LoadBalancerSourceRanges
	filteredCIDRs := filterValidCIDRs(logger, sourceRanges)

	if len(filteredCIDRs) == 0 {
		// No source ranges: unbind ACL from listeners, then delete IP group if exists.
		if existingACLID != "" {
			// Unbind IP group from all listeners before deleting it.
			if elbID != "" {
				if err := r.unbindACLFromAllListeners(logger, elbID); err != nil {
					return "", "", "", false, err
				}
			}
			if err := huaweicloud.DeleteIPGroup(r.ELBClient, existingACLID); err != nil {
				if !huaweicloud.IsNotFoundError(err) {
					return "", "", "", false, err
				}
			}
		}
		return "", "", "", false, nil
	}

	// Has source ranges: create or update IP group.
	ipGroupID := existingACLID
	ipGroupName := "acl-" + svc.Namespace + "-" + svc.Name
	if ipGroupID == "" {
		var findErr error
		ipGroupID, findErr = huaweicloud.FindIPGroupByName(r.ELBClient, ipGroupName)
		if findErr != nil {
			return "", "", "", false, findErr
		}
	}
	if ipGroupID != "" {
		if err := huaweicloud.UpdateIPGroup(r.ELBClient, ipGroupID, ipGroupName, filteredCIDRs); err != nil {
			return "", "", "", false, err
		}
	} else {
		newID, createErr := huaweicloud.CreateIPGroup(r.ELBClient, ipGroupName, "ACL for "+svc.Name, filteredCIDRs)
		if createErr != nil {
			return "", "", "", false, createErr
		}
		ipGroupID = newID
	}
	// Bind IP group to all listeners on the ELB.
	if elbID != "" {
		if err := r.bindACLToAllListeners(logger, elbID, ipGroupID); err != nil {
			return "", "", "", false, err
		}
	}
	return ipGroupID, "on", "white", true, nil
}

// bindACLToAllListeners binds the IP group to all listeners on the specified ELB.
func (r *ServiceReconciler) bindACLToAllListeners(logger logr.Logger, elbID, ipGroupID string) error {
	listeners, err := huaweicloud.ListListeners(r.ELBClient, elbID)
	if err != nil {
		return fmt.Errorf("listing listeners for ACL binding: %w", err)
	}
	for _, l := range listeners {
		if err := huaweicloud.UpdateListenerACL(r.ELBClient, l.ID, ipGroupID, true); err != nil {
			return fmt.Errorf("binding ACL to listener %s: %w", l.ID, err)
		}
	}
	logger.Info("ACL bound to listeners", "elbID", elbID, "ipGroupID", ipGroupID, "listenerCount", len(listeners))
	return nil
}

// unbindACLFromAllListeners disables the ACL on all listeners on the specified ELB.
func (r *ServiceReconciler) unbindACLFromAllListeners(logger logr.Logger, elbID string) error {
	listeners, err := huaweicloud.ListListeners(r.ELBClient, elbID)
	if err != nil {
		return fmt.Errorf("listing listeners for ACL unbinding: %w", err)
	}
	for _, l := range listeners {
		if err := huaweicloud.UpdateListenerACL(r.ELBClient, l.ID, "", false); err != nil {
			return fmt.Errorf("unbinding ACL from listener %s: %w", l.ID, err)
		}
	}
	logger.Info("ACL unbound from listeners", "elbID", elbID, "listenerCount", len(listeners))
	return nil
}

// NodeBackend represents a ready node's backend info for ELB member creation.
// In multi-AZ clusters, each node may be in a different subnet, so we track
// the node's virsubnet ID alongside its IP.
type NodeBackend struct {
	IP          string
	VirsubnetID string // from node.kubernetes.io/subnetid label
}

// getNodeBackends returns ready nodes' internal IPs and their virsubnet IDs.
// Returns an error if the Kubernetes API call fails, so callers can skip member
// sync rather than accidentally clearing all members with an empty list.
//
// When the Service uses externalTrafficPolicy: Local, only nodes hosting a
// Ready/NotReady endpoint pod for that Service are returned -- otherwise ELB
// health checks fail on nodes whose NodePort does not forward to a local pod
// (under Local policy, kube-proxy does not proxy NodePort traffic to other
// nodes). When the policy is Cluster (default), all ready nodes are returned.
func (r *ServiceReconciler) getNodeBackends(ctx context.Context, svc *corev1.Service) ([]NodeBackend, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	allBackends := make([]NodeBackend, 0, len(nodeList.Items))
	nodeByIP := make(map[string]corev1.Node, len(nodeList.Items))
	for _, node := range nodeList.Items {
		if !isNodeReady(&node) {
			continue
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				allBackends = append(allBackends, NodeBackend{
					IP:          addr.Address,
					VirsubnetID: node.Labels["node.kubernetes.io/subnetid"],
				})
				nodeByIP[addr.Address] = node
				break
			}
		}
	}

	// externalTrafficPolicy: Cluster (default) -> all ready nodes are members.
	if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
		return allBackends, nil
	}

	// externalTrafficPolicy: Local -> only nodes hosting an endpoint pod for
	// this Service. Query the Endpoints object (same name/namespace as the
	// Service) and keep only backends whose node name is in the endpoint set.
	allowList, err := r.localEndpointNodeSet(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("listing endpoints for local-traffic Service %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	if len(allowList) == 0 {
		// No endpoints yet (e.g. during startup or after scale-down). Return
		// empty so caller registers zero members rather than stale ones.
		return nil, nil
	}
	filtered := make([]NodeBackend, 0, len(allBackends))
	for _, be := range allBackends {
		node, ok := nodeByIP[be.IP]
		if !ok {
			continue
		}
		if allowList[node.Name] {
			filtered = append(filtered, be)
		}
	}
	return filtered, nil
}

// localEndpointNodeSet returns the set of node names that host at least one
// endpoint (Ready or NotReady) for the given Service. Used to filter ELB
// members when externalTrafficPolicy: Local is set.
func (r *ServiceReconciler) localEndpointNodeSet(ctx context.Context, svc *corev1.Service) (map[string]bool, error) {
	eps := &corev1.Endpoints{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(svc), eps); err != nil {
		return nil, err
	}
	nodes := make(map[string]bool)
	for _, subset := range eps.Subsets {
		for _, addr := range subset.Addresses {
			if addr.NodeName != nil && *addr.NodeName != "" {
				nodes[*addr.NodeName] = true
			}
		}
		// Include NotReady addresses so members are registered during pod
		// startup before the endpoint flips to ready -- ELB health check will
		// mark the member healthy once the pod is ready.
		for _, addr := range subset.NotReadyAddresses {
			if addr.NodeName != nil && *addr.NodeName != "" {
				nodes[*addr.NodeName] = true
			}
		}
	}
	return nodes, nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	svcPredicate := predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			svc, ok := e.Object.(*corev1.Service)
			if !ok {
				return false
			}
			return shouldReconcileService(svc)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			svcOld, ok := e.ObjectOld.(*corev1.Service)
			if !ok {
				return false
			}
			svcNew, ok := e.ObjectNew.(*corev1.Service)
			if !ok {
				return false
			}
			// Only reconcile on spec changes (generation bump). Annotation-only
			// updates (e.g. our own writes, operator annotation syncs) do not
			// change generation and should not trigger reconcilation.
			// Service type changes (e.g. ClusterIP→LoadBalancer) are caught
			// by shouldReconcileService on both old and new.
			if svcNew.Generation == svcOld.Generation {
				return false
			}
			return shouldReconcileService(svcOld) || shouldReconcileService(svcNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			svc, ok := e.Object.(*corev1.Service)
			if !ok {
				return false
			}
			return controllerutil.ContainsFinalizer(svc, huaweicloud.AnnotationELBCleanupFinalizer) ||
				controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			svc, ok := e.Object.(*corev1.Service)
			if !ok {
				return false
			}
			return shouldReconcileService(svc)
		},
	}

	// Node event handler: map node changes to all OpenEverest LoadBalancer Services.
	nodeHandler := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		svcList := &corev1.ServiceList{}
		if err := mgr.GetClient().List(ctx, svcList); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for i := range svcList.Items {
			svc := &svcList.Items[i]
			if shouldReconcileService(svc) && hasManagedELBID(svc) {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: svc.Namespace,
						Name:      svc.Name,
					},
				})
			}
		}
		return requests
	})

	// Endpoint event handler: map endpoint changes to the owning Service so that
	// member sync runs when pods are scheduled/descheduled under Local policy.
	// Without this, a pod moving to a new node would not trigger member re-sync
	// until the next periodic reconcile.
	epHandler := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		svc := &corev1.Service{}
		if err := mgr.GetClient().Get(ctx, client.ObjectKeyFromObject(obj), svc); err != nil {
			return nil
		}
		if !shouldReconcileService(svc) || !hasManagedELBID(svc) {
			return nil
		}
		// Only Local-policy Services care about endpoint changes for member sync.
		if svc.Spec.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyLocal {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: svc.Namespace, Name: svc.Name,
		}}}
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(svcPredicate).
		Watches(&corev1.Node{}, nodeHandler).
		Watches(&corev1.Endpoints{}, epHandler).
		Owns(&v1alpha1.ELBBinding{}).
		Complete(r)
}

func paramsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func buildUpdateOption(current, lastKnown map[string]string) huaweicloud.UpdateELBOption {
	opt := huaweicloud.UpdateELBOption{}

	if current[huaweicloud.LBCBandwidthSizeAnnotation] != lastKnown[huaweicloud.LBCBandwidthSizeAnnotation] {
		if v, err := strconv.Atoi(current[huaweicloud.LBCBandwidthSizeAnnotation]); err == nil && v > 0 {
			opt.BandwidthSize = int32(v)
		}
	}

	if current[huaweicloud.LBCBandwidthChargeModeAnnotation] != lastKnown[huaweicloud.LBCBandwidthChargeModeAnnotation] {
		opt.BandwidthChargeMode = current[huaweicloud.LBCBandwidthChargeModeAnnotation]
	}

	return opt
}

func buildCompositeParams(lbcParams map[string]string, sourceRanges []string) map[string]string {
	composite := make(map[string]string)
	for k, v := range lbcParams {
		composite[k] = v
	}
	if sourceRangesJSON, err := json.Marshal(sourceRanges); err == nil {
		composite[sourceRangesKey] = string(sourceRangesJSON)
	}
	return composite
}

// filterValidCIDRs filters out invalid CIDR prefixes and logs skipped entries.
func filterValidCIDRs(logger logr.Logger, cidrs []string) []string {
	if len(cidrs) == 0 {
		return nil
	}
	var valid []string
	for _, cidr := range cidrs {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			logger.Info("skipping invalid source range CIDR", "cidr", cidr, "error", err)
		} else {
			valid = append(valid, cidr)
		}
	}
	return valid
}

