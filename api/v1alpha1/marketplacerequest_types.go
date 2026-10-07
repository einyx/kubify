package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type MarketplaceRequestPhase string

const (
	MarketplaceRequestPending      MarketplaceRequestPhase = "Pending"
	MarketplaceRequestProvisioning MarketplaceRequestPhase = "Provisioning"
	MarketplaceRequestReady        MarketplaceRequestPhase = "Ready"
	MarketplaceRequestFailed       MarketplaceRequestPhase = "Failed"
)

// MarketplaceRequestSpec describes an activated purchase that should provision
// a Foundation tenant. SubscriptionID is the provider's idempotency key.
type MarketplaceRequestSpec struct {
	// +kubebuilder:validation:Enum=azure
	Provider       string `json:"provider"`
	SubscriptionID string `json:"subscriptionId"`
	OfferID        string `json:"offerId"`
	PlanID         string `json:"planId"`
	// +kubebuilder:validation:Minimum=1
	Quantity         int32  `json:"quantity,omitempty"`
	ProviderTenantID string `json:"providerTenantId,omitempty"`
	PurchaserEmail   string `json:"purchaserEmail"`
	SubscriptionName string `json:"subscriptionName,omitempty"`
	// +kubebuilder:validation:Enum=lite;full
	Template string `json:"template"`
}

type MarketplaceRequestStatus struct {
	Phase              MarketplaceRequestPhase `json:"phase,omitempty"`
	DemoRequestRef     string                  `json:"demoRequestRef,omitempty"`
	Tenant             string                  `json:"tenant,omitempty"`
	URL                string                  `json:"url,omitempty"`
	Message            string                  `json:"message,omitempty"`
	ObservedGeneration int64                   `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=marketreq
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.planId`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.status.tenant`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MarketplaceRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MarketplaceRequestSpec   `json:"spec,omitempty"`
	Status            MarketplaceRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MarketplaceRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MarketplaceRequest `json:"items"`
}

func init() { SchemeBuilder.Register(&MarketplaceRequest{}, &MarketplaceRequestList{}) }
