package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// StackRelease tracks the deployment state of a single Stack component.
// One StackRelease is created per component per Stack, named <stack>-<component>.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Stack",type=string,JSONPath=`.spec.stackRef`
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=`.spec.component`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Revision",type=integer,JSONPath=`.status.revision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StackRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StackReleaseSpec   `json:"spec,omitempty"`
	Status StackReleaseStatus `json:"status,omitempty"`
}

type StackReleaseSpec struct {
	// StackRef is the name of the parent Stack.
	StackRef string `json:"stackRef"`
	// Component is the component name within the Stack.
	Component string `json:"component"`
}

type StackReleaseStatus struct {
	// Phase is the current deployment phase: Deploying, Ready, Failed.
	// +optional
	Phase ComponentPhase `json:"phase,omitempty"`
	// Revision is the Helm release revision.
	// +optional
	Revision int `json:"revision,omitempty"`
	// Message is a human-readable status description.
	// +optional
	Message string `json:"message,omitempty"`
	// LastDeployedAt is when the component was last successfully deployed.
	// +optional
	LastDeployedAt *metav1.Time `json:"lastDeployedAt,omitempty"`
}

// +kubebuilder:object:root=true
type StackReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StackRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StackRelease{}, &StackReleaseList{})
}
