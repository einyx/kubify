package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type OAuthSecretReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// OAuthProvider describes a centrally managed OAuth control-plane identity.
// It is cluster-scoped because one provider can provision applications for
// many tenant namespaces.
type OAuthProviderSpec struct {
	// +kubebuilder:validation:Enum=Auth0
	Type string `json:"type"`
	// Issuer is the public issuer, preferably an Auth0 custom domain.
	// +kubebuilder:validation:Pattern=`^https://`
	Issuer string `json:"issuer"`
	// ManagementAPIURL is normally the canonical Auth0 tenant /api/v2 URL,
	// even when Issuer is a custom login domain.
	// +kubebuilder:validation:Pattern=`^https://`
	ManagementAPIURL string `json:"managementAPIURL"`
	// ManagementAudience defaults to ManagementAPIURL with a trailing slash.
	// +optional
	ManagementAudience string `json:"managementAudience,omitempty"`
	// TokenURL defaults to <issuer>/oauth/token.
	// +optional
	TokenURL                       string               `json:"tokenURL,omitempty"`
	ManagementCredentialsSecretRef OAuthSecretReference `json:"managementCredentialsSecretRef"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=oauthprovider
type OAuthProvider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OAuthProviderSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true
type OAuthProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OAuthProvider `json:"items"`
}

type OAuthApplicationSpec struct {
	ProviderRef string `json:"providerRef"`
	// +kubebuilder:validation:Enum=regular_web;spa;native;non_interactive
	ApplicationType string `json:"applicationType"`
	// +optional
	Callbacks []string `json:"callbacks,omitempty"`
	// +optional
	LogoutURLs []string `json:"logoutURLs,omitempty"`
	// +optional
	WebOrigins []string `json:"webOrigins,omitempty"`
	// +optional
	AllowedOrigins  []string                              `json:"allowedOrigins,omitempty"`
	SecretTargetRef OAuthApplicationSecretTargetReference `json:"secretTargetRef"`
}

type OAuthApplicationSecretTargetReference struct {
	Name string `json:"name"`
}

type OAuthApplicationStatus struct {
	// ProviderApplicationID is non-secret and may safely be exposed in status.
	// +optional
	ProviderApplicationID string `json:"providerApplicationID,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=oauthapp
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.providerRef`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type OAuthApplication struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              OAuthApplicationSpec   `json:"spec,omitempty"`
	Status            OAuthApplicationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type OAuthApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OAuthApplication `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OAuthProvider{}, &OAuthProviderList{}, &OAuthApplication{}, &OAuthApplicationList{})
}
