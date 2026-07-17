/*
Copyright 2025 The Kubo Authors.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeploymentMode selects how components are deployed.
// +kubebuilder:validation:Enum=Direct;Flux
type DeploymentMode string

const (
	// DeploymentModeDirect deploys charts via the operator's embedded Helm engine.
	DeploymentModeDirect DeploymentMode = "Direct"
	// DeploymentModeFlux compiles components into Flux HelmRelease objects
	// and delegates deployment to Flux helm-controller.
	DeploymentModeFlux DeploymentMode = "Flux"
)

// StackSpec defines a namespaced instance of a stack.
type StackSpec struct {
	// StackRef names the cluster-scoped StackDefinition to deploy.
	// Mutually exclusive with Inline.
	// +optional
	StackRef string `json:"stackRef,omitempty"`

	// Inline embeds the stack definition directly, so a Stack is a single
	// self-contained YAML. Mutually exclusive with StackRef.
	// +optional
	Inline *StackDefinitionSpec `json:"inline,omitempty"`

	// Bundle pulls one OCI artifact (for example oci://ghcr.io/org/product-bundle:tag)
	// and installs Helm charts from its charts.tgz layer. Usable alone, or with
	// StackRef/Inline to select which charts to install. Charts missing from the
	// bundle keep their ChartRef.
	// +optional
	Bundle *BundleSource `json:"bundle,omitempty"`

	// Exclude names bundle charts to skip. Only applied when installing directly
	// from a Bundle (no StackRef/Inline).
	// +optional
	Exclude []string `json:"exclude,omitempty"`

	// Mode selects the deployment strategy. Defaults to Direct.
	// +kubebuilder:default=Direct
	// +optional
	Mode DeploymentMode `json:"mode,omitempty"`

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

	// Operators installs cluster-wide operators once, outside any tenant namespace.
	// +optional
	Operators *ClusterOperators `json:"operators,omitempty"`

	// SeedVault mirrors keys from a plain k8s Secret into the per-tenant
	// Vault KV store on every reconcile. Secret values never appear in the
	// Stack spec — the source Secret holds them (create it out-of-band, e.g.
	// `kubectl create secret generic product-seeds --from-env-file=.env`).
	// +optional
	SeedVault *VaultSeed `json:"seedVault,omitempty"`
}

// VaultSeed configures automatic seeding of the per-tenant Vault.
type VaultSeed struct {
	// SourceSecret is a Secret in the Stack namespace whose keys are copied
	// into Vault. +kubebuilder:validation:MinLength=1
	SourceSecret string `json:"sourceSecret"`

	// Entries map Vault KV paths to the source Secret keys to copy.
	// +kubebuilder:validation:MinItems=1
	Entries []VaultSeedEntry `json:"entries"`
}

// VaultSeedEntry copies a set of source Secret keys into one Vault path.
type VaultSeedEntry struct {
	// Path is the KV v2 path (relative to the secret/ mount), e.g. frontend/auth0.
	// +kubebuilder:validation:MinLength=1
	Path string `json:"path"`

	// Keys are the source Secret keys copied to this path.
	// +kubebuilder:validation:MinItems=1
	Keys []string `json:"keys"`
}

// ClusterOperators selects shared operators. They are installed once in the
// operators namespace and watch every Stack namespace. Tenant charts named
// vault-operator or spark-operator are never installed into the Stack namespace.
type ClusterOperators struct {
	// Vault installs bank-vaults vault-operator.
	// +optional
	Vault bool `json:"vault,omitempty"`
	// Spark installs kubeflow spark-operator.
	// +optional
	Spark bool `json:"spark,omitempty"`
	// Istio installs the istiod control plane (CRDs are installed by the
	// operator at startup). Stacks deploy their own VirtualServices.
	// +optional
	Istio bool `json:"istio,omitempty"`
	// Kafka installs the Strimzi kafka-operator. Stacks then run Kafka via
	// Kafka/KafkaTopic custom resources instead of a per-stack kafka chart.
	// +optional
	Kafka bool `json:"kafka,omitempty"`
	// Postgres installs the kubegres operator. Stacks then run Postgres via
	// Postgres custom resources instead of a per-stack postgres chart.
	// +optional
	Postgres bool `json:"postgres,omitempty"`
	// AgentFW deploys the kubo-native agent firewall proxy into each tenant
	// namespace. Agents route egress through it for DLP and injection scanning.
	// +optional
	AgentFW bool `json:"agentFW,omitempty"`
}

// BundleSource is an OCI artifact that carries the stack's charts.
type BundleSource struct {
	// URL is an OCI reference, including the tag or digest.
	// Example: oci://ghcr.io/einyx/product-bundle:0.0.8
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// SecretRef is a kubernetes.io/dockerconfigjson Secret used to pull the bundle.
	// Required for private registries such as GHCR.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// ComponentStatus is the observed state of one component of the stack.
type ComponentStatus struct {
	Name string `json:"name"`

	// Phase: Pending | Deploying | Ready | Failed | Degraded
	Phase ComponentPhase `json:"phase"`

	// Scope: Namespaced (installed in the Stack namespace) or Cluster
	// (installed once in the operators namespace).
	// +kubebuilder:validation:Enum=Namespaced;Cluster
	// +optional
	Scope ComponentScope `json:"scope,omitempty"`

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

const (
	ComponentPhasePending   ComponentPhase = "Pending"
	ComponentPhaseDeploying ComponentPhase = "Deploying"
	ComponentPhaseReady     ComponentPhase = "Ready"
	ComponentPhaseFailed    ComponentPhase = "Failed"
	ComponentPhaseDegraded  ComponentPhase = "Degraded"
)

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
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Stack deploys a named StackDefinition into its own namespace. The
// abstraction is product-agnostic: product, dai, product-ai or any
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
