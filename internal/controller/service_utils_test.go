package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsLoadBalancerService(t *testing.T) {
	lb := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	if !isLoadBalancerService(lb) {
		t.Error("expected true for LoadBalancer type")
	}

	clusterIP := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	if isLoadBalancerService(clusterIP) {
		t.Error("expected false for ClusterIP type")
	}
}

func TestHasELBID(t *testing.T) {
	withID := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"kubernetes.io/elb.id": "elb-12345",
			},
		},
	}
	if !hasELBID(withID) {
		t.Error("expected true when annotation is present")
	}

	withoutID := &corev1.Service{}
	if hasELBID(withoutID) {
		t.Error("expected false when annotations are nil")
	}

	other := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"other": "value",
			},
		},
	}
	if hasELBID(other) {
		t.Error("expected false when annotation is absent")
	}
}

func TestHasAutocreate(t *testing.T) {
	withAuto := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"kubernetes.io/elb.autocreate": "{}",
			},
		},
	}
	if !hasAutocreate(withAuto) {
		t.Error("expected true when autocreate annotation is present")
	}

	withoutAuto := &corev1.Service{}
	if hasAutocreate(withoutAuto) {
		t.Error("expected false when annotations are nil")
	}
}

func TestHasLBCParams(t *testing.T) {
	withParams := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"huawei-elb.io/public":         "true",
				"huawei-elb.io/bandwidth-size": "10",
			},
		},
	}
	if !hasLBCParams(withParams) {
		t.Error("expected true when huawei-elb.io/ annotations are present")
	}

	withoutParams := &corev1.Service{}
	if hasLBCParams(withoutParams) {
		t.Error("expected false when annotations are nil")
	}

	otherAnnotations := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"kubernetes.io/elb.id": "elb-123",
			},
		},
	}
	if hasLBCParams(otherAnnotations) {
		t.Error("expected false when no huawei-elb.io/ annotations")
	}
}

func TestGetLBCParams(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"huawei-elb.io/public":         "true",
				"huawei-elb.io/bandwidth-size": "20",
				"kubernetes.io/elb.id":         "elb-123",
				"other":                        "value",
			},
		},
	}

	params := getLBCParams(svc)

	if len(params) != 2 {
		t.Errorf("expected 2 huawei-elb.io/ params, got %d", len(params))
	}
	if params["huawei-elb.io/public"] != "true" {
		t.Errorf("expected public=true, got %s", params["huawei-elb.io/public"])
	}
	if params["huawei-elb.io/bandwidth-size"] != "20" {
		t.Errorf("expected bandwidth-size=20, got %s", params["huawei-elb.io/bandwidth-size"])
	}

	noAnnotations := &corev1.Service{}
	emptyParams := getLBCParams(noAnnotations)
	if len(emptyParams) != 0 {
		t.Errorf("expected empty params for nil annotations, got %d", len(emptyParams))
	}
}

func TestIsOpenEverestService(t *testing.T) {
	matching := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "percona-xtradb-cluster-operator",
			},
		},
	}
	if !isOpenEverestService(matching) {
		t.Error("expected true for OpenEverest-managed service")
	}

	noLabels := &corev1.Service{}
	if isOpenEverestService(noLabels) {
		t.Error("expected false for no labels")
	}

	wrongLabel := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "helm",
			},
		},
	}
	if isOpenEverestService(wrongLabel) {
		t.Error("expected false for non-OpenEverest label value")
	}
}

func TestHasForeignCloudServiceAnnotations(t *testing.T) {
	gke := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"networking.gke.io/load-balancer-type": "Internal",
			},
		},
	}
	if !hasForeignCloudServiceAnnotations(gke) {
		t.Error("expected true for GKE annotations")
	}

	aws := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"service.beta.kubernetes.io/aws-load-balancer-type": "nlb",
			},
		},
	}
	if !hasForeignCloudServiceAnnotations(aws) {
		t.Error("expected true for AWS annotations")
	}

	none := &corev1.Service{}
	if hasForeignCloudServiceAnnotations(none) {
		t.Error("expected false for nil annotations")
	}

	huawei := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"huawei-elb.io/public": "true",
			},
		},
	}
	if hasForeignCloudServiceAnnotations(huawei) {
		t.Error("expected false for Huawei annotations")
	}
}

func TestShouldReconcileService(t *testing.T) {
	validService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "percona-xtradb-cluster-operator",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	if !shouldReconcileService(validService) {
		t.Error("expected true for valid LoadBalancer+Everest service")
	}

	clusterIPService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "percona-xtradb-cluster-operator",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	if shouldReconcileService(clusterIPService) {
		t.Error("expected false for ClusterIP type")
	}

	withELBID := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "percona-xtradb-cluster-operator",
			},
			Annotations: map[string]string{
				"kubernetes.io/elb.id": "elb-existing",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	if shouldReconcileService(withELBID) {
		t.Error("expected false when ELB ID already present")
	}

	foreignService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "percona-xtradb-cluster-operator",
			},
			Annotations: map[string]string{
				"service.beta.kubernetes.io/aws-load-balancer-type": "nlb",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	if shouldReconcileService(foreignService) {
		t.Error("expected false for foreign cloud annotations")
	}

	nonEverest := &corev1.Service{
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
	if shouldReconcileService(nonEverest) {
		t.Error("expected false for non-Everest service")
	}
}
