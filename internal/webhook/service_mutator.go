package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	// LoadBalancerClassValue is the class we inject so CCE CCM completely
	// skips the Service (0 events, no status clearing, no elb.id writing).
	LoadBalancerClassValue = "huawei-elb.io/direct-api"

	ccmAutocreateAnnotation = "kubernetes.io/elb.autocreate"
	ccmELBIDAnnotation      = "kubernetes.io/elb.id"
)

// ServiceMutator is a mutating admission webhook that injects
// spec.loadBalancerClass on LoadBalancer Services not managed by CCM.
//
// Problem: CCE CCM watches all type: LoadBalancer Services. If a Service
// has no kubernetes.io/elb.id annotation, CCM continuously clears
// status.loadBalancer.ingress. The controller rewrites it, CCM clears it
// again -- an infinite race.
//
// Solution: spec.loadBalancerClass is the K8s-standard signal for "this LB
// is not yours". CCE CCM respects it: seeing a non-matching class, CCM
// completely skips the Service (verified: 0 events, no status clearing).
//
// Constraint: loadBalancerClass can only be set at Service CREATE time
// (K8s API server rejects patches from nil to a value). PSMDB operator
// creates Services without it. This webhook injects it at creation time.
type ServiceMutator struct{}

// Handle implements admission.Handler.
func (m *ServiceMutator) Handle(_ context.Context, req admission.Request) admission.Response {
	// Only mutate CREATE operations.
	if req.Operation != admissionv1.Create {
		return admission.Allowed("not a create operation")
	}

	// Decode the Service from the raw admission request.
	svc := &corev1.Service{}
	if err := json.Unmarshal(req.Object.Raw, svc); err != nil {
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding service: %w", err))
	}

	// Only inject for LoadBalancer type.
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return admission.Allowed("not a LoadBalancer service")
	}

	// Skip if already has a loadBalancerClass.
	if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass != "" {
		return admission.Allowed("already has loadBalancerClass")
	}

	// Skip CCM-managed services (they have kubernetes.io/elb.* annotations).
	if svc.Annotations != nil {
		if _, ok := svc.Annotations[ccmAutocreateAnnotation]; ok {
			return admission.Allowed("CCM-managed service (has elb.autocreate)")
		}
		if _, ok := svc.Annotations[ccmELBIDAnnotation]; ok {
			return admission.Allowed("CCM-managed service (has elb.id)")
		}
	}

	// Inject loadBalancerClass so CCM completely skips this Service.
	lbClass := LoadBalancerClassValue
	svc.Spec.LoadBalancerClass = &lbClass

	marshaled, err := json.Marshal(svc)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling service: %w", err))
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}
