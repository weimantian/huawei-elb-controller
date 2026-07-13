package controller

import (
"context"
"encoding/json"
"net/netip"
"sort"
"strconv"
"time"

"github.com/go-logr/logr"
elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
corev1 "k8s.io/api/core/v1"
ctrl "sigs.k8s.io/controller-runtime"
"sigs.k8s.io/controller-runtime/pkg/client"
"sigs.k8s.io/controller-runtime/pkg/event"
"sigs.k8s.io/controller-runtime/pkg/log"
"sigs.k8s.io/controller-runtime/pkg/predicate"

"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

type ServiceReconciler struct {
	client.Client
	ELBClient       *elb.ElbClient
	NetworkDetector *huaweicloud.NetworkDetector
	Creds           *huaweicloud.Credentials
}

const (
	lastKnownParamsAnnotation = "huawei-elb.io/last-known-params"
	elbClassAnnotation        = "kubernetes.io/elb.class"
	reclaimPolicyAnnotation   = "kubernetes.io/elb.instance-reclaim-policy"
	serviceRequeue            = 5 * time.Minute
	serviceRetryRequeue       = 10 * time.Second

	aclIDAnnotation     = "kubernetes.io/elb.acl-id"
	aclStatusAnnotation = "kubernetes.io/elb.acl-status"
	aclTypeAnnotation   = "kubernetes.io/elb.acl-type"
	sourceRangesKey     = "source-ranges"
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

	if hasELBID(svc) && !hasAutocreate(svc) {
// Skip Services with elb.id that were NOT created by autocreate (legacy CCM binding)
return ctrl.Result{}, nil
}

	if hasForeignCloudServiceAnnotations(svc) {
		return ctrl.Result{}, nil
	}

	if !isOpenEverestService(svc) {
		return ctrl.Result{}, nil
	}

if !svc.DeletionTimestamp.IsZero() {
logger.Info("Service being deleted, cleaning up ACL IP group", "service", svc.Name)
if ipGroupID := svc.Annotations[aclIDAnnotation]; ipGroupID != "" {
if err := huaweicloud.DeleteIPGroup(r.ELBClient, ipGroupID); err != nil {
logger.Error(err, "deleting ACL IP group on Service deletion")
} else {
logger.Info("ACL IP group deleted", "ipGroupID", ipGroupID)
}
}
// ELB cleanup handled by CCM via reclaim-policy
return ctrl.Result{}, nil
}

	logger.Info("Reconciling Service", "name", svc.Name, "namespace", svc.Namespace)

	if hasAutocreate(svc) {
		return r.reconcileUpdate(ctx, logger, svc)
	}
	return r.reconcileCreate(ctx, logger, svc)
}

func (r *ServiceReconciler) reconcileCreate(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, error) {
	lbcParams := getLBCParams(svc)

	vpcID, subnetID, azs, err := r.NetworkDetector.Detect(ctx, r.Client)
	if err != nil {
		logger.Error(err, "network detection failed")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	autocreateJSON, err := huaweicloud.BuildAutocreateJSON(lbcParams, vpcID, subnetID, azs, svc.Namespace+"-"+svc.Name)
	if err != nil {
		logger.Error(err, "building autocreate JSON failed")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	original := svc.DeepCopy()

	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}
	svc.Annotations[huaweicloud.CCEAutocreateAnnotation] = autocreateJSON
	svc.Annotations[elbClassAnnotation] = "union"
	svc.Annotations[reclaimPolicyAnnotation] = "alwaysDelete"

// ACL handling
sourceRanges := svc.Spec.LoadBalancerSourceRanges
filteredCIDRs := filterValidCIDRs(logger, sourceRanges)
if len(filteredCIDRs) > 0 {
ipGroupName := "acl-" + svc.Namespace + "-" + svc.Name
ipGroupID, err := huaweicloud.CreateIPGroup(r.ELBClient, ipGroupName, "ACL for "+svc.Name, filteredCIDRs)
			if err != nil {
				logger.Error(err, "creating IP group for ACL")
				return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
} else {
svc.Annotations[aclIDAnnotation] = ipGroupID
svc.Annotations[aclStatusAnnotation] = "on"
svc.Annotations[aclTypeAnnotation] = "white"
}
} else {
svc.Annotations[aclStatusAnnotation] = "off"
}

compositeParams := make(map[string]string)
	for k, v := range lbcParams {
		compositeParams[k] = v
	}
	if sourceRangesJSON, err := json.Marshal(filteredCIDRs); err == nil {
		compositeParams[sourceRangesKey] = string(sourceRangesJSON)
	}

	paramsJSON, err := json.Marshal(compositeParams)
	if err == nil {
		svc.Annotations[lastKnownParamsAnnotation] = string(paramsJSON)
	}

	if err := r.Patch(ctx, svc, client.MergeFrom(original)); err != nil {
		logger.Error(err, "patching Service with autocreate annotations")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	logger.Info("Injected autocreate annotation on Service",
		"service", svc.Name, "subnetID", subnetID, "azs", azs)
	return ctrl.Result{RequeueAfter: serviceRequeue}, nil
}

func (r *ServiceReconciler) reconcileUpdate(ctx context.Context, logger logr.Logger, svc *corev1.Service) (ctrl.Result, error) {
	currentParams := getLBCParams(svc)

	lastKnownJSON := svc.Annotations[lastKnownParamsAnnotation]
	lastKnownParams := make(map[string]string)
	if lastKnownJSON != "" {
		_ = json.Unmarshal([]byte(lastKnownJSON), &lastKnownParams)
	}

	lastSourceRangesJSON := lastKnownParams[sourceRangesKey]
	delete(lastKnownParams, sourceRangesKey)

	var lastSourceRanges []string
	if lastSourceRangesJSON != "" {
		_ = json.Unmarshal([]byte(lastSourceRangesJSON), &lastSourceRanges)
	}
currentSourceRanges := svc.Spec.LoadBalancerSourceRanges
validCurrentSourceRanges := filterValidCIDRs(logger, currentSourceRanges)
aclChanged := !sourceRangesEqual(lastSourceRanges, validCurrentSourceRanges)
if aclChanged {
			if len(validCurrentSourceRanges) == 0 {
				// Source ranges cleared: delete old IP group and disable ACL
				if oldID := svc.Annotations[aclIDAnnotation]; oldID != "" {
					if err := huaweicloud.DeleteIPGroup(r.ELBClient, oldID); err != nil {
						logger.Error(err, "deleting old IP group for ACL")
					}
				}
				// Always clean up stale ACL annotations
				delete(svc.Annotations, aclIDAnnotation)
				delete(svc.Annotations, aclTypeAnnotation)
				svc.Annotations[aclStatusAnnotation] = "off"
} else {
ipGroupName := "acl-" + svc.Namespace + "-" + svc.Name
			ipGroupID := svc.Annotations[aclIDAnnotation]
			if ipGroupID == "" {
				var findErr error
				ipGroupID, findErr = huaweicloud.FindIPGroupByName(r.ELBClient, ipGroupName)
				if findErr != nil {
					logger.Error(findErr, "finding IP group for ACL")
				}
			}
			if ipGroupID != "" {
				if err := huaweicloud.UpdateIPGroup(r.ELBClient, ipGroupID, ipGroupName, validCurrentSourceRanges); err != nil {
					logger.Error(err, "updating IP group for ACL")
				} else {
					svc.Annotations[aclStatusAnnotation] = "on"
				}
			} else {
				newID, err := huaweicloud.CreateIPGroup(r.ELBClient, ipGroupName, "ACL for "+svc.Name, validCurrentSourceRanges)
				if err != nil {
					logger.Error(err, "creating IP group for ACL")
				} else {
					svc.Annotations[aclIDAnnotation] = newID
					svc.Annotations[aclStatusAnnotation] = "on"
					svc.Annotations[aclTypeAnnotation] = "white"
				}
			}
		}
	}

	paramsChanged := !paramsEqual(currentParams, lastKnownParams)

	if !paramsChanged && !aclChanged {
		return ctrl.Result{RequeueAfter: serviceRequeue}, nil
	}

	if paramsChanged {
		logger.Info("LBC params changed, updating ELB", "service", svc.Name)

		elbID := svc.Annotations[huaweicloud.AnnotationELBID]
		if elbID == "" {
			elbName := "cce-lb-" + svc.Namespace + "-" + svc.Name
			info, err := huaweicloud.FindELBByName(r.ELBClient, elbName)
if err != nil {
logger.Error(err, "finding ELB by name")
}
			if info != nil {
				elbID = info.ID
			}
		}

		if elbID != "" {
			opt := buildUpdateOption(currentParams, lastKnownParams)
			if err := huaweicloud.UpdateELB(r.ELBClient, elbID, opt, r.Creds); err != nil {
				logger.Error(err, "updating ELB", "elbID", elbID)
				return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
			}
			logger.Info("ELB updated", "elbID", elbID)
		}
	}

	original := svc.DeepCopy()
	compositeParams := make(map[string]string)
	for k, v := range currentParams {
		compositeParams[k] = v
	}
	if sourceRangesJSON, err := json.Marshal(validCurrentSourceRanges); err == nil {
		compositeParams[sourceRangesKey] = string(sourceRangesJSON)
	}

	paramsJSON, err := json.Marshal(compositeParams)
	if err == nil {
		if svc.Annotations == nil {
			svc.Annotations = make(map[string]string)
		}
		svc.Annotations[lastKnownParamsAnnotation] = string(paramsJSON)
	} else {
		logger.Error(err, "marshaling current params")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	if err := r.Patch(ctx, svc, client.MergeFrom(original)); err != nil {
		logger.Error(err, "patching last-known params")
		return ctrl.Result{RequeueAfter: serviceRetryRequeue}, nil
	}

	return ctrl.Result{RequeueAfter: serviceRequeue}, nil
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
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			svc, ok := e.Object.(*corev1.Service)
			if !ok {
				return false
			}
			return shouldReconcileService(svc)
		},
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(svcPredicate).
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

	if current["huawei-elb.io/name"] != lastKnown["huawei-elb.io/name"] {
		// Name changes are not supported by the current UpdateELB API
		// This would require adding Name to UpdateELBOption
	}

	return opt
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

func sourceRangesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sortedA := make([]string, len(a))
	sortedB := make([]string, len(b))
	copy(sortedA, a)
	copy(sortedB, b)
	sort.Strings(sortedA)
	sort.Strings(sortedB)
	for i := range sortedA {
		if sortedA[i] != sortedB[i] {
			return false
		}
	}
	return true
}
