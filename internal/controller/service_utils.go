package controller

import (
	corev1 "k8s.io/api/core/v1"
	"strings"
)

const (
	serviceLabelManagedBy      = "app.kubernetes.io/managed-by"
	serviceLabelManagedByValue = "percona-xtradb-cluster-operator"
)

func isLoadBalancerService(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer
}

func hasELBID(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	_, ok := svc.Annotations["kubernetes.io/elb.id"]
	return ok
}

func hasAutocreate(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	_, ok := svc.Annotations["kubernetes.io/elb.autocreate"]
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
	return labels[serviceLabelManagedBy] == serviceLabelManagedByValue
}

func hasForeignCloudServiceAnnotations(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return false
	}
	prefixes := []string{
		"service.beta.kubernetes.io/aws-",
		"service.beta.kubernetes.io/azure-",
		"service.beta.kubernetes.io/alibaba-",
		"cloud.google.com/",
		"networking.gke.io/",
	}
	for key := range svc.Annotations {
		for _, prefix := range prefixes {
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
	if hasELBID(svc) && !hasAutocreate(svc) {
// Skip legacy ELB ID bindings, but allow Plan 2 autocreate Services
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
