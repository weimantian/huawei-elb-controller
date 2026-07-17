package controller

import (
	corev1 "k8s.io/api/core/v1"
	"strings"
	"time"

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

const (
	serviceLabelManagedBy = "app.kubernetes.io/managed-by"

	// Shared requeue intervals used by both ServiceReconciler and LoadBalancerConfigReconciler.
	sharedHealthyRequeue = 5 * time.Minute  // periodic requeue when ELB is ACTIVE/Ready
	sharedRetryRequeue   = 10 * time.Second // temporary errors (network, throttling)
)

// openeverestOperators lists the managed-by values for all OpenEverest engine operators.
var openeverestOperators = map[string]bool{
	"percona-xtradb-cluster-operator": true, // MySQL / PXC
	"percona-server-mongodb-operator": true, // MongoDB / PSMDB
	"percona-postgresql-operator":     true, // PostgreSQL
}

func isLoadBalancerService(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer
}

// hasELBIDManaged checks if the Service has our huawei-elb.io/elb-id annotation,
// indicating we already created an ELB for it (Plan B direct-API mode).
func hasManagedELBID(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	_, ok := svc.Annotations[huaweicloud.AnnotationELBID]
	return ok
}

func hasLBCParams(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	for key := range svc.Annotations {
		if strings.HasPrefix(key, "huawei-elb.io/") {
			return true
		}
	}
	return false
}

func getLBCParams(svc *corev1.Service) map[string]string {
	params := make(map[string]string)
	if svc.Annotations == nil {
		return params
	}
	controllerKeys := map[string]bool{
		"huawei-elb.io/last-known-params": true,
	}
	for key, value := range svc.Annotations {
		if strings.HasPrefix(key, "huawei-elb.io/") && !controllerKeys[key] {
			params[key] = value
		}
	}
	return params
}

func isOpenEverestService(svc *corev1.Service) bool {
	labels := svc.GetLabels()
	if labels == nil {
		return false
	}
	return openeverestOperators[labels[serviceLabelManagedBy]]
}

// ForeignCloudAnnotationPrefixes are annotation key prefixes that indicate
// a Service or LBC targets a different cloud provider (AWS, GKE, Azure,
// Alibaba). The controller skips these to avoid creating Huawei Cloud ELBs
// for resources meant for other clouds (e.g. OpenEverest's built-in eks-default).
var ForeignCloudAnnotationPrefixes = []string{
	"service.beta.kubernetes.io/aws-",
	"service.beta.kubernetes.io/azure-",
	"service.beta.kubernetes.io/alibaba-",
	"cloud.google.com/",
	"networking.gke.io/",
}

func hasForeignCloudServiceAnnotations(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	for key := range svc.Annotations {
		for _, prefix := range ForeignCloudAnnotationPrefixes {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
	}
	return false
}

func shouldReconcileService(svc *corev1.Service) bool {
	if !isLoadBalancerService(svc) {
		return false
	}
	// Skip Services with legacy kubernetes.io/elb.id (CCM-managed, not ours)
	if hasLegacyELBID(svc) {
		return false
	}
	// Skip Services with legacy kubernetes.io/elb.autocreate (old Plan 2 CCM mode)
	if hasLegacyAutocreate(svc) {
		return false
	}
	if hasForeignCloudServiceAnnotations(svc) {
		return false
	}
	if !isOpenEverestService(svc) {
		return false
	}
	return true
}

// hasLegacyELBID checks for the old CCM-managed kubernetes.io/elb.id annotation.
// Services with this annotation were created by the old autocreate controller
// or by CCM directly. We skip them to avoid conflicts.
func hasLegacyELBID(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	_, ok := svc.Annotations["kubernetes.io/elb.id"]
	return ok
}

// hasLegacyAutocreate checks for the old kubernetes.io/elb.autocreate annotation.
func hasLegacyAutocreate(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	_, ok := svc.Annotations["kubernetes.io/elb.autocreate"]
	return ok
}
