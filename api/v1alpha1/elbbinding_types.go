package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=elbb
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceName`
// +kubebuilder:printcolumn:name="ELB-ID",type=string,JSONPath=`.status.elbID`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ELBBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ELBBindingSpec   `json:"spec,omitempty"`
	Status ELBBindingStatus `json:"status,omitempty"`
}

type ELBBindingSpec struct {
	// ServiceName is the owning Service (same namespace). Immutable.
	ServiceName string `json:"serviceName"`
	// ServiceUID guards against Service name reuse. Immutable.
	ServiceUID string `json:"serviceUID"`
}

type ELBBindingStatus struct {
	ELBID              string            `json:"elbID,omitempty"`
	ACLID              string            `json:"aclID,omitempty"`
	ACLStatus          string            `json:"aclStatus,omitempty"` // "on" | "off"
	ACLType            string            `json:"aclType,omitempty"`   // "white"
	LastKnownParams    map[string]string `json:"lastKnownParams,omitempty"`
	IngressIP          string            `json:"ingressIP,omitempty"`
	Phase              string            `json:"phase,omitempty"` // Provisioning | Ready | Deleting
	ObservedGeneration int64             `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
type ELBBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ELBBinding `json:"items"`
}

const (
	PhaseProvisioning = "Provisioning"
	PhaseReady        = "Ready"
	PhaseDeleting     = "Deleting"

	ACLStatusOn  = "on"
	ACLStatusOff = "off"
	ACLTypeWhite = "white"
)

func init() { SchemeBuilder.Register(&ELBBinding{}, &ELBBindingList{}) }
