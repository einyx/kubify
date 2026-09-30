/*
Copyright 2025 The Kubo Authors.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StackSpec defines a namespaced instance of a stack.
type StackSpec struct {
	// StackRef names the cluster-scoped StackDefinition to deploy.
	// +kubebuilder:validation:MinLength=1
	StackRef string `json:"stackRef"`

	// Version pins the StackDefinition to a content version. Empty = latest.
	// +optional
	Version string `json:"version,omitempty"`

	// Values overrides applied on top of the StackDefinition defaults for
	// every component (deep merge, component path: components.<name>.<key>).
	// +optional
	Values apiextensionsv1.JSON `json:"values,omitempty"`

	// ComponentValues holds per-component Helm value overrides.
	// +optional
	ComponentValues map[string]apiextensionsv1.JSON `json:"componentValues,omitempty"`

	// Paused stops reconciliation without deleting deployed components.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// ComponentStatus is the observed state of one component of the stack.
type ComponentStatus struct {
	Name string `json:"name"`

	// Phase: Pending | Deploying | Ready | Failed | Degraded
	Phase ComponentPhase `json:"phase"`

	// Revision of the Helm release.
	// +optional
	Revision int `json:"revision,omitempty"`

	// Message carries the last error or readiness detail.
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	LastDeployed *metav1.Time `json:"lastDeployed,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Deploying;Ready;Failed;Degraded
type ComponentPhase string

// StackStatus defines the observed state of Stack.
type StackStatus struct {
	// Phase: Pending | Progressing | Ready | Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// +optional
	Components []ComponentStatus `json:"components,omitempty"`

	// ObservedGeneration is the generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=stack
// +kubebuilder:printcolumn:name="StackRef",type=string,JSONPath=`.spec.stackRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Stack deploys a named StackDefinition into its own namespace. The
// abstraction is product-agnostic: foundation, dai, foundation-ai or any
// future product is just a StackDefinition.
type Stack struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StackSpec   `json:"spec,omitempty"`
	Status StackStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StackList contains a list of Stack.
type StackList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Stack `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Stack{}, &StackList{})
}
