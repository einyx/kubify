package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type GitWritebackPhase string

const (
	GitWritebackPending         GitWritebackPhase = "Pending"
	GitWritebackRunning         GitWritebackPhase = "Running"
	GitWritebackPullRequestOpen GitWritebackPhase = "PullRequestOpen"
	GitWritebackApplied         GitWritebackPhase = "Applied"
	GitWritebackConflict        GitWritebackPhase = "Conflict"
	GitWritebackFailed          GitWritebackPhase = "Failed"
)

type WritebackTarget struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// GitWritebackRequestSpec describes an allowlisted mutation to a Git-managed Stack.
type GitWritebackRequestSpec struct {
	Target WritebackTarget `json:"target"`
	// +optional
	FeatureFlags map[string]string `json:"featureFlags,omitempty"`
	// +optional
	ImageTags map[string]string `json:"imageTags,omitempty"`
	// ExpectedFeatureFlags enables optimistic conflict detection.
	// +optional
	ExpectedFeatureFlags map[string]string `json:"expectedFeatureFlags,omitempty"`
	// ExpectedImageTags enables optimistic conflict detection.
	// +optional
	ExpectedImageTags map[string]string `json:"expectedImageTags,omitempty"`
	// +optional
	ChartVersions map[string]string `json:"chartVersions,omitempty"`
	// ExpectedChartVersions enables optimistic conflict detection.
	// +optional
	ExpectedChartVersions map[string]string `json:"expectedChartVersions,omitempty"`
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
}

type GitWritebackRequestStatus struct {
	// +optional
	Phase GitWritebackPhase `json:"phase,omitempty"`
	// +optional
	Repository string `json:"repository,omitempty"`
	// +optional
	Path string `json:"path,omitempty"`
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	PullRequestURL string `json:"pullRequestURL,omitempty"`
	// +optional
	Commit string `json:"commit,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=gitwb
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="PR",type=string,JSONPath=`.status.pullRequestURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GitWritebackRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GitWritebackRequestSpec   `json:"spec,omitempty"`
	Status            GitWritebackRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GitWritebackRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitWritebackRequest `json:"items"`
}

func init() { SchemeBuilder.Register(&GitWritebackRequest{}, &GitWritebackRequestList{}) }
