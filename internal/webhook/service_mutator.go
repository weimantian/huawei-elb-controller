package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
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
// (K8s API server rejects patches from nil to a value). DB operators
// (PSMDB, PXC) create Services without it. This webhook injects it at
// creation time.
//
// Update protection: some DB operators (PXC upgrade, PSMDB
// ensureExternalServices) construct Service objects from templates that
// don't include loadBalancerClass, then call Update -- which submits
// loadBalancerClass=null and is rejected ("may not change once set"). The
// webhook also intercepts UPDATE and restores the injected class so the
// operator's Update succeeds (allowing sourceRanges and other spec
// changes to apply). PostgreSQL operator preserves loadBalancerClass on
// Update, so it is unaffected.
type ServiceMutator struct{}

// Handle implements admission.Handler.
func (m *ServiceMutator) Handle(_ context.Context, req admission.Request) admission.Response {
	logger := log.Log.WithName("service-mutator").WithValues(
		"namespace", req.Namespace,
		"name", req.Name,
		"operation", req.Operation)

	// Decode the new Service from the raw admission request.
	svc := &corev1.Service{}
	if err := json.Unmarshal(req.Object.Raw, svc); err != nil {
		logger.Error(err, "decoding service")
		return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding service: %w", err))
	}

	// Only care about LoadBalancer type Services.
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		logger.Info("allowed: not a LoadBalancer service")
		return admission.Allowed("not a LoadBalancer service")
	}

	switch req.Operation {
	case admissionv1.Create:
		// Skip if already has a loadBalancerClass.
		if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass != "" {
			logger.Info("allowed: already has loadBalancerClass", "loadBalancerClass", *svc.Spec.LoadBalancerClass)
			return admission.Allowed("already has loadBalancerClass")
		}
		// Skip CCM-managed services (they have kubernetes.io/elb.* annotations).
		if hasCCMAnnotations(svc.Annotations) {
			logger.Info("allowed: CCM-managed service")
			return admission.Allowed("CCM-managed service")
		}
		// Inject loadBalancerClass so CCM completely skips this Service.
		lbClass := LoadBalancerClassValue
		svc.Spec.LoadBalancerClass = &lbClass
		logger.Info("injecting loadBalancerClass on CREATE", "loadBalancerClass", lbClass)

	case admissionv1.Update:
		// If the operator preserved loadBalancerClass, nothing to do.
		if svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass != "" {
			logger.Info("allowed: loadBalancerClass preserved", "loadBalancerClass", *svc.Spec.LoadBalancerClass)
			return admission.Allowed("loadBalancerClass preserved")
		}
		// Check if the old Service had our loadBalancerClass injected.
		// DB operators (PXC, PSMDB) construct Service objects from templates
		// that don't include loadBalancerClass, then call Update -- which
		// submits loadBalancerClass=null and is rejected by the API server
		// ("may not change once set"). We restore the value so the Update
		// succeeds, allowing sourceRanges and other spec changes to apply.
		oldSvc := &corev1.Service{}
		if err := json.Unmarshal(req.OldObject.Raw, oldSvc); err != nil {
			logger.Error(err, "decoding old service")
			return admission.Errored(http.StatusBadRequest, fmt.Errorf("decoding old service: %w", err))
		}
		if oldSvc.Spec.LoadBalancerClass == nil || *oldSvc.Spec.LoadBalancerClass != LoadBalancerClassValue {
			logger.Info("allowed: old service did not have our loadBalancerClass")
			return admission.Allowed("old service did not have our loadBalancerClass")
		}
		lbClass := LoadBalancerClassValue
		svc.Spec.LoadBalancerClass = &lbClass
		logger.Info("restoring loadBalancerClass on UPDATE", "loadBalancerClass", lbClass)

	default:
		logger.Info("allowed: not a create or update operation")
		return admission.Allowed("not a create or update operation")
	}

	marshaled, err := json.Marshal(svc)
	if err != nil {
		logger.Error(err, "marshaling service")
		return admission.Errored(http.StatusInternalServerError, fmt.Errorf("marshaling service: %w", err))
	}

	logger.Info("patching service to apply loadBalancerClass")
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
}

// hasCCMAnnotations returns true if the Service has CCM-managed annotations
// (kubernetes.io/elb.autocreate or kubernetes.io/elb.id).
func hasCCMAnnotations(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	if _, ok := annotations[ccmAutocreateAnnotation]; ok {
		return true
	}
	if _, ok := annotations[ccmELBIDAnnotation]; ok {
		return true
	}
	return false
}
