package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func strPtr(s string) *string { return &s }

func makeLBService(name string, class *string, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "everest", Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: class,
		},
	}
}

func makeClusterIPService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "everest"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
	}
}

func svcRaw(t *testing.T, svc *corev1.Service) runtime.RawExtension {
	t.Helper()
	raw, err := json.Marshal(svc)
	if err != nil {
		t.Fatalf("marshaling service: %v", err)
	}
	return runtime.RawExtension{Raw: raw}
}

// patchContainsClass checks that the response is allowed and the patch
// operations include setting spec.loadBalancerClass to our value.
func patchContainsClass(t *testing.T, resp admission.Response) {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %s", resp.Result.Message)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected non-empty patches")
	}
	found := false
	for _, p := range resp.Patches {
		if strings.Contains(p.Path, "loadBalancerClass") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("patches do not contain loadBalancerClass: %+v", resp.Patches)
	}
}

// allowedNoPatch checks that the response is allowed with no mutation.
func allowedNoPatch(t *testing.T, resp admission.Response) {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %s", resp.Result.Message)
	}
	if len(resp.Patches) > 0 {
		t.Fatalf("expected no patches, got: %+v", resp.Patches)
	}
}

// --- CREATE tests ---

func TestHandleCreateInjectsClass(t *testing.T) {
	m := &ServiceMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    svcRaw(t, makeLBService("test-svc", nil, nil)),
		},
	}
	patchContainsClass(t, m.Handle(context.Background(), req))
}

func TestHandleCreateSkipsAlreadyHasClass(t *testing.T) {
	m := &ServiceMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    svcRaw(t, makeLBService("test-svc", strPtr("other.io/class"), nil)),
		},
	}
	allowedNoPatch(t, m.Handle(context.Background(), req))
}

func TestHandleCreateSkipsCCMManaged(t *testing.T) {
	m := &ServiceMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object: svcRaw(t, makeLBService("test-svc", nil, map[string]string{
				"kubernetes.io/elb.id": "some-elb-id",
			})),
		},
	}
	allowedNoPatch(t, m.Handle(context.Background(), req))
}

func TestHandleCreateSkipsNonLoadBalancer(t *testing.T) {
	m := &ServiceMutator{}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Object:    svcRaw(t, makeClusterIPService("test-svc")),
		},
	}
	allowedNoPatch(t, m.Handle(context.Background(), req))
}

// --- UPDATE tests ---

func TestHandleUpdateRestoresClearedClass(t *testing.T) {
	// Core fix: DB operator (PXC/PSMDB) submits Service without loadBalancerClass,
	// webhook restores it so the Update succeeds.
	m := &ServiceMutator{}
	oldSvc := makeLBService("test-svc", strPtr(LoadBalancerClassValue), nil)
	newSvc := makeLBService("test-svc", nil, nil)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Object:    svcRaw(t, newSvc),
			OldObject: svcRaw(t, oldSvc),
		},
	}
	patchContainsClass(t, m.Handle(context.Background(), req))
}

func TestHandleUpdateAllowsPreservedClass(t *testing.T) {
	// PG operator preserves loadBalancerClass on Update -- webhook should not
	// produce a redundant patch.
	m := &ServiceMutator{}
	oldSvc := makeLBService("test-svc", strPtr(LoadBalancerClassValue), nil)
	newSvc := makeLBService("test-svc", strPtr(LoadBalancerClassValue), nil)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Object:    svcRaw(t, newSvc),
			OldObject: svcRaw(t, oldSvc),
		},
	}
	allowedNoPatch(t, m.Handle(context.Background(), req))
}

func TestHandleUpdateSkipsNonOurClass(t *testing.T) {
	// Old Service had a different class -- not our concern, leave it alone.
	m := &ServiceMutator{}
	oldSvc := makeLBService("test-svc", strPtr("other.io/class"), nil)
	newSvc := makeLBService("test-svc", nil, nil)
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Object:    svcRaw(t, newSvc),
			OldObject: svcRaw(t, oldSvc),
		},
	}
	allowedNoPatch(t, m.Handle(context.Background(), req))
}

func TestHandleUpdateSkipsNonLoadBalancer(t *testing.T) {
	// Service type changed from LoadBalancer to ClusterIP -- not our concern.
	m := &ServiceMutator{}
	oldSvc := makeLBService("test-svc", strPtr(LoadBalancerClassValue), nil)
	newSvc := makeClusterIPService("test-svc")
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Object:    svcRaw(t, newSvc),
			OldObject: svcRaw(t, oldSvc),
		},
	}
	allowedNoPatch(t, m.Handle(context.Background(), req))
}
