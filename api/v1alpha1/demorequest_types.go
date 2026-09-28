/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DemoRequestPhase tracks the lifecycle of a website-originated demo tenant.
type DemoRequestPhase string

const (
	// DemoRequestPending means the request is admitted but not yet provisioning.
	DemoRequestPending DemoRequestPhase = "Pending"
	// DemoRequestProvisioning means the tenant Stack is deploying.
	DemoRequestProvisioning DemoRequestPhase = "Provisioning"
	// DemoRequestReady means the tenant Stack is Ready and the requester
	// has been (or is being) emailed their tenant URL.
	DemoRequestReady DemoRequestPhase = "Ready"
	// DemoRequestFailed means provisioning or notification failed; the
	// message carries the reason.
	DemoRequestFailed DemoRequestPhase = "Failed"
	// DemoRequestExpired means the TTL elapsed and the tenant was cleaned up.
	DemoRequestExpired DemoRequestPhase = "Expired"
)

// DemoRequestSpec is a website "request demo" submission reconciled into a
// tenant Stack provisioned from a portal template.
type DemoRequestSpec struct {
	// Email of the requester; validated on admission. The Ready email is
	// sent here.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[^\s@]+@[^\s@]+\.[^\s@]+$`
	Email string `json:"email"`

	// Company name; preferred source for the tenant slug.
	// +optional
	// +kubebuilder:validation:MaxLength=40
	Company string `json:"company,omitempty"`

	// Target identifies the cluster or operator instance that should
	// provision this request. When empty the controller on the local
	// cluster picks it up; a non-empty value lets multi-cluster setups
	// filter on it (e.g. "prod-eu", "staging").
	// +optional
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9\-]*[a-z0-9])?$`
	Target string `json:"target,omitempty"`

	// Template id from the portal registry (e.g. "full", "lite").
	// +optional
	// +kubebuilder:validation:MaxLength=32
	Template string `json:"template,omitempty"`

	// TTL is how long the tenant lives after approval before automatic
	// cleanup. Defaults to 72h. Zero means no auto-cleanup.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// Approved releases provisioning: nothing deploys until an operator
	// flips this in the portal. The TTL clock starts at approval, so the
	// 72h window is the tenant's actual lifetime.
	// +optional
	Approved bool `json:"approved,omitempty"`
}

// DemoRequestStatus mirrors the provisioned tenant back to the requester flow.
type DemoRequestStatus struct {
	// Phase of the demo lifecycle.
	// +optional
	Phase DemoRequestPhase `json:"phase,omitempty"`
	// Tenant is the namespace slug the Stack was created in.
	// +optional
	Tenant string `json:"tenant,omitempty"`
	// URL is the public tenant URL once known.
	// +optional
	URL string `json:"url,omitempty"`
	// Message carries the failure reason when Phase=Failed.
	// +optional
	Message string `json:"message,omitempty"`
	// NotifiedAt records when the requester was emailed (RFC3339).
	// +optional
	NotifiedAt string `json:"notifiedAt,omitempty"`
	// ApprovedAt records when the request was approved (RFC3339); the
	// TTL deadline counts from here.
	// +optional
	ApprovedAt string `json:"approvedAt,omitempty"`
	// ExpiresAt records the TTL deadline (RFC3339).
	// +optional
	ExpiresAt string `json:"expiresAt,omitempty"`
	// ObservedGeneration tracks spec changes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=demoreq
// +kubebuilder:printcolumn:name="Email",type=string,JSONPath=`.spec.email`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.status.tenant`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DemoRequest is the user-facing API of the demo-tenant lifecycle: the
// website submits one per "request demo" form, and the operator provisions
// the tenant from a portal template, emails the requester their URL when the
// Stack is Ready, and cleans everything up when the TTL expires.
type DemoRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DemoRequestSpec   `json:"spec,omitempty"`
	Status DemoRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DemoRequestList contains a list of DemoRequest.
type DemoRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DemoRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DemoRequest{}, &DemoRequestList{})
}
