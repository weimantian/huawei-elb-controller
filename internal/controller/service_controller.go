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

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

type ServiceReconciler struct {
	client.Client
	ELBClient       *elb.ElbClient
	NetworkDetector *huaweicloud.NetworkDetector
	Creds           *huaweicloud.Credentials
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

	// CRITICAL: Write elbID annotation + finalizer immediately after ELB creation.
	// This ensures that if subsequent steps (listener/pool/AACL/status) fail or the
	// process crashes, the next reconcile will find the elbID and route to
	// reconcileUpdate, which can complete provisioning via syncListenerStacks.
	// Without this, a crash after ELB creation but before annotation write would
	// orphan the ELB (leak) and create a duplicate on retry.
	compositeParams := buildCompositeParams(lbcParams, filterValidCIDRs(logger, svc.Spec.LoadBalancerSourceRanges))
	paramsJSON, _ := json.Marshal(compositeParams)

	if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations[huaweicloud.AnnotationELBID] = info.ID
		latest.Annotations[lastKnownParamsAnnotation] = string(paramsJSON)
		controllerutil.AddFinalizer(latest, huaweicloud.AnnotationELBCleanupFinalizer)
		return nil
	}); err != nil {
		logger.Error(err, "patching Service with ELB ID and finalizer")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Get node backends for member creation (NodePort mode, multi-AZ aware).
	backends, err := r.getNodeBackends(ctx)
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
	if err := r.ensureACL(ctx, logger, svc); err != nil {
		logger.Error(err, "ensuring ACL, will retry")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// Persist ACL annotations (ensureACL modifies svc.Annotations in-memory).
	if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		for _, key := range []string{aclIDAnnotation, aclStatusAnnotation, aclTypeAnnotation} {
			if v, ok := svc.Annotations[key]; ok {
				latest.Annotations[key] = v
			} else {
				delete(latest.Annotations, key)
			}
		}
		if controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer) {
			controllerutil.AddFinalizer(latest, aclCleanupFinalizer)
		}
		return nil
	}); err != nil {
		logger.Error(err, "patching ACL annotations")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
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
	elbID := svc.Annotations[huaweicloud.AnnotationELBID]
	if elbID == "" {
		logger.Info("ELB ID missing, falling back to create")
		return r.reconcileCreate(ctx, logger, svc)
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
	lastKnownJSON := svc.Annotations[lastKnownParamsAnnotation]
	lastKnownParams := make(map[string]string)
	if lastKnownJSON != "" {
		_ = json.Unmarshal([]byte(lastKnownJSON), &lastKnownParams)
	}
	delete(lastKnownParams, sourceRangesKey)

	if !paramsEqual(currentParams, lastKnownParams) {
		opt := buildUpdateOption(currentParams, lastKnownParams)
		// Call UpdateELB if either bandwidth size or charge mode changed.
		// BandwidthSize=0 with non-empty ChargeMode means only charge mode changed.
		if opt.BandwidthSize > 0 || opt.BandwidthChargeMode != "" {
			if err := huaweicloud.UpdateELB(r.ELBClient, elbID, opt, r.Creds); err != nil {
				logger.Error(err, "updating ELB params", "elbID", elbID)
				return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
			}
			logger.Info("ELB params updated", "elbID", elbID, "bandwidthSize", opt.BandwidthSize, "chargeMode", opt.BandwidthChargeMode)
		}
	}

	// 4. Handle ACL changes (source ranges).
	if err := r.ensureACL(ctx, logger, svc); err != nil {
		logger.Error(err, "ensuring ACL")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	// 5. Persist last-known-params.
	compositeParams := buildCompositeParams(currentParams, filterValidCIDRs(logger, svc.Spec.LoadBalancerSourceRanges))
	paramsJSON, _ := json.Marshal(compositeParams)
	if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
		if latest.Annotations == nil {
			latest.Annotations = make(map[string]string)
		}
		latest.Annotations[lastKnownParamsAnnotation] = string(paramsJSON)
		for _, key := range []string{aclIDAnnotation, aclStatusAnnotation, aclTypeAnnotation} {
			if v, ok := svc.Annotations[key]; ok {
				latest.Annotations[key] = v
			} else {
				delete(latest.Annotations, key)
			}
		}
		if controllerutil.ContainsFinalizer(svc, aclCleanupFinalizer) {
			controllerutil.AddFinalizer(latest, aclCleanupFinalizer)
		} else {
			controllerutil.RemoveFinalizer(latest, aclCleanupFinalizer)
		}
		return nil
	}); err != nil {
		logger.Error(err, "patching last-known params")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}
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

	backends, err := r.getNodeBackends(ctx)
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

	backends, err := r.getNodeBackends(ctx)
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
		logger.Info("Pool members synced", "pool", pool.ID, "memberCount", len(desired))
	}

	return nil
}

// reconcileDelete deletes the ELB and ACL IP group, then removes finalizers.
func (r *ServiceReconciler) reconcileDelete(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, error) {
	// Delete ELB if we own it.
	if controllerutil.ContainsFinalizer(svc, huaweicloud.AnnotationELBCleanupFinalizer) {
		elbID := svc.Annotations[huaweicloud.AnnotationELBID]
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
		if ipGroupID := svc.Annotations[aclIDAnnotation]; ipGroupID != "" {
			if err := huaweicloud.DeleteIPGroup(r.ELBClient, ipGroupID); err != nil {
				if !huaweicloud.IsNotFoundError(err) {
					logger.Error(err, "deleting ACL IP group, will retry", "ipGroupID", ipGroupID)
					return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
				}
			}
		}
		if err := r.patchWithRetry(ctx, client.ObjectKeyFromObject(svc), func(latest *corev1.Service) error {
			controllerutil.RemoveFinalizer(latest, aclCleanupFinalizer)
			return nil
		}); err != nil {
			return ctrl.Result{}, err
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
	return huaweicloud.DeleteELB(r.ELBClient, elbID)
}

// ensureACL creates/updates/deletes the ACL IP group based on loadBalancerSourceRanges.
func (r *ServiceReconciler) ensureACL(ctx context.Context, logger logr.Logger, svc *corev1.Service) error {
	sourceRanges := svc.Spec.LoadBalancerSourceRanges
	filteredCIDRs := filterValidCIDRs(logger, sourceRanges)

	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}


	if len(filteredCIDRs) == 0 {
		// No source ranges: disable ACL, delete IP group if exists.
		if oldID := svc.Annotations[aclIDAnnotation]; oldID != "" {
			if err := huaweicloud.DeleteIPGroup(r.ELBClient, oldID); err != nil {
				if !huaweicloud.IsNotFoundError(err) {
					return err
				}
			}
		}
		delete(svc.Annotations, aclIDAnnotation)
		delete(svc.Annotations, aclTypeAnnotation)
		svc.Annotations[aclStatusAnnotation] = "off"
		controllerutil.RemoveFinalizer(svc, aclCleanupFinalizer)
		return nil
	}

	// Has source ranges: create or update IP group.
	ipGroupName := "acl-" + svc.Namespace + "-" + svc.Name
	ipGroupID := svc.Annotations[aclIDAnnotation]
	if ipGroupID == "" {
		var findErr error
		ipGroupID, findErr = huaweicloud.FindIPGroupByName(r.ELBClient, ipGroupName)
		if findErr != nil {
			return findErr
		}
	}
	if ipGroupID != "" {
		if err := huaweicloud.UpdateIPGroup(r.ELBClient, ipGroupID, ipGroupName, filteredCIDRs); err != nil {
			return err
		}
		svc.Annotations[aclStatusAnnotation] = "on"
	} else {
		newID, err := huaweicloud.CreateIPGroup(r.ELBClient, ipGroupName, "ACL for "+svc.Name, filteredCIDRs)
		if err != nil {
			return err
		}
		svc.Annotations[aclIDAnnotation] = newID
		svc.Annotations[aclStatusAnnotation] = "on"
		svc.Annotations[aclTypeAnnotation] = "white"
		controllerutil.AddFinalizer(svc, aclCleanupFinalizer)
	}
	return nil
}

// NodeBackend represents a ready node's backend info for ELB member creation.
// In multi-AZ clusters, each node may be in a different subnet, so we track
// the node's virsubnet ID alongside its IP.
type NodeBackend struct {
	IP          string
	VirsubnetID string // from node.kubernetes.io/subnetid label
}

// getNodeBackends returns all ready nodes' internal IPs and their virsubnet IDs.
// Returns an error if the Kubernetes API call fails, so callers can skip member
// sync rather than accidentally clearing all members with an empty list.
func (r *ServiceReconciler) getNodeBackends(ctx context.Context) ([]NodeBackend, error) {
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	var backends []NodeBackend
	for _, node := range nodeList.Items {
		if !isNodeReady(&node) {
			continue
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				backends = append(backends, NodeBackend{
					IP:          addr.Address,
					VirsubnetID: node.Labels["node.kubernetes.io/subnetid"],
				})
				break
			}
		}
	}
	return backends, nil
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

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(svcPredicate).
		Watches(&corev1.Node{}, nodeHandler).
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

