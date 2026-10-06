package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type MarketplaceSubscriptionPhase string

const (
	MarketplaceSubscriptionPending     MarketplaceSubscriptionPhase = "Pending"
	MarketplaceSubscriptionProvisioning MarketplaceSubscriptionPhase = "Provisioning"
	MarketplaceSubscriptionActive       MarketplaceSubscriptionPhase = "Active"
	MarketplaceSubscriptionSuspended    MarketplaceSubscriptionPhase = "Suspended"
	MarketplaceSubscriptionUnsubscribed MarketplaceSubscriptionPhase = "Unsubscribed"
	MarketplaceSubscriptionFailed       MarketplaceSubscriptionPhase = "Failed"
)

// MarketplaceSubscriptionSpec binds one Microsoft Marketplace entitlement to
// one Kubo tenant Stack. The Marketplace subscription ID is immutable.
type MarketplaceSubscriptionSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SubscriptionID string `json:"subscriptionId"`
	// +kubebuilder:validation:Required
	OfferID string `json:"offerId"`
	// +kubebuilder:validation:Required
	PlanID string `json:"planId"`
	// +kubebuilder:validation:Minimum=1
	// +optional
	Quantity int32 `json:"quantity,omitempty"`
	// +kubebuilder:validation:Required
	AzureTenantID string `json:"azureTenantId"`
	// +optional
	PurchaserEmail string `json:"purchaserEmail,omitempty"`
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	TenantNamespace string `json:"tenantNamespace"`
	// Portal template selected by the Marketplace plan mapping.
	// +kubebuilder:validation:Enum=lite;full
	Template string `json:"template"`
}

type MarketplaceSubscriptionStatus struct {
	// +optional
	Phase MarketplaceSubscriptionPhase `json:"phase,omitempty"`
	// MarketplaceStatus is Microsoft's last validated lifecycle status.
	// +optional
	MarketplaceStatus string `json:"marketplaceStatus,omitempty"`
	// +optional
	StackRef string `json:"stackRef,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mktplace
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.planId`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenantNamespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type MarketplaceSubscription struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec MarketplaceSubscriptionSpec `json:"spec,omitempty"`
	Status MarketplaceSubscriptionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type MarketplaceSubscriptionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items []MarketplaceSubscription `json:"items"`
}

func init() { SchemeBuilder.Register(&MarketplaceSubscription{}, &MarketplaceSubscriptionList{}) }
