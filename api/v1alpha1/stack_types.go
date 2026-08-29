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

	// ExtraBundles lists additional OCI bundle artifacts to merge into the chart
	// map alongside Bundle. Charts in later entries override earlier ones.
	// +optional
	ExtraBundles []BundleSource `json:"extraBundles,omitempty"`

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

	// SecretsRef lists secrets to copy from kubo-system into the tenant
	// namespace on every reconcile. Use this to seed per-tenant credentials
	// (e.g. Auth0 client secrets) without storing them in the Stack spec.
	// +optional
	SecretsRef []SecretMapping `json:"secretsRef,omitempty"`

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

	// GitRef points to a Flux GitRepository in the same namespace.
	// Kubo reads status.artifact.url to fetch charts without cloning git directly,
	// reusing Flux's existing auth secrets.
	// +optional
	GitRef *GitRefSource `json:"gitRef,omitempty"`

	// VirtualService defines an Istio VirtualService the operator manages for
	// this Stack. All HTTP routes are reconciled on every cycle, so extra `---`
	// documents in the Stack file are not needed.
	// +optional
	VirtualService *StackVirtualService `json:"virtualService,omitempty"`
}

// StackVirtualService describes a managed Istio VirtualService.
type StackVirtualService struct {
	// Name of the VirtualService. Defaults to "<stack-name>-routes".
	// +optional
	Name string `json:"name,omitempty"`
	// Gateway reference in "namespace/name" or just "name" form.
	// +kubebuilder:validation:MinLength=1
	Gateway string `json:"gateway"`
	// Host is the virtual host (DNS name).
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// AdditionalHosts are extra virtual hosts served by the same routes
	// (e.g. the canonical tenant URL alongside a legacy one).
	// +optional
	AdditionalHosts []string `json:"additionalHosts,omitempty"`
	// HTTP is the ordered list of route rules, passed verbatim into the VS spec.
	// Use the same structure as Istio's HTTPRoute.
	// +kubebuilder:validation:MinItems=1
	HTTP []apiextensionsv1.JSON `json:"http"`
}

// GitRefSource references a git repository as a chart source.
type GitRefSource struct {
	// URL is the HTTPS or SSH git repository URL.
	// Example: https://github.com/org/repo.git
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// Ref is the branch, tag, or commit SHA to check out. Defaults to "main".
	// +optional
	Ref string `json:"ref,omitempty"`

	// Provider enables cloud-native auth. Only "azure" is supported — uses
	// workload identity (IMDS) to obtain an Azure DevOps access token.
	// When set, SecretRef is ignored.
	// +optional
	Provider string `json:"provider,omitempty"`

	// SecretRef names a Secret in the same namespace used for git authentication.
	// For HTTPS: must contain "username" and "password" keys.
	// For SSH: must contain an "identity" key (private key PEM).
	// Ignored when Provider is set.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// Path is the file inside the cloned repo to use as the charts archive.
	// Must be a .tgz containing Helm charts. Defaults to "charts.tgz".
	// +optional
	Path string `json:"path,omitempty"`
}

// VaultSeed configures automatic seeding of the per-tenant Vault.
type VaultSeed struct {
	// SourceSecret is a Secret in the Stack namespace whose keys are copied
	// into Vault. Required when Entries is set.
	// +optional
	SourceSecret string `json:"sourceSecret,omitempty"`

	// Entries map Vault KV paths to the source Secret keys to copy.
	// +optional
	Entries []VaultSeedEntry `json:"entries,omitempty"`

	// Static copies pre-generated SHARED credentials (Auth0 clients, vendor
	// API keys) from canonical kubo-system Secrets into tenant Secrets.
	// Auth0 clients cannot be generated — they exist in the Auth0 tenant and
	// are shared across demo deployments. Created only if the target Secret
	// does not exist; never overwritten.
	// +optional
	Static []VaultSeedSecret `json:"static,omitempty"`

	// Generated creates fresh RANDOM per-tenant credentials (Trino JWT/S3
	// keys, DB passwords, session secrets). Every tenant gets its own values.
	// Created only if the target Secret does not exist; for Secrets that
	// already exist (e.g. created by Static), only keys that are still
	// missing are added — existing values are never overwritten.
	// +optional
	Generated []VaultSeedSecret `json:"generated,omitempty"`
}

// VaultSeedSecret describes one kubo-system Secret kubo creates at bootstrap.
type VaultSeedSecret struct {
	// Name of the kubo-system Secret, e.g. product-c-trino-s3-credentials.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Type of the Secret. Defaults to Opaque; use kubernetes.io/tls for
	// generated key pairs.
	// +kubebuilder:validation:Enum=Opaque;kubernetes.io/tls
	// +kubebuilder:default=Opaque
	// +optional
	Type corev1.SecretType `json:"type,omitempty"`

	// Literal key/values written verbatim (non-secret identifiers, domains,
	// audiences).
	// +optional
	Literal map[string]string `json:"literal,omitempty"`

	// Generate creates cryptographically random values for these keys
	// (per-tenant credentials).
	// +optional
	Generate map[string]GeneratedKey `json:"generate,omitempty"`

	// CopyFrom copies keys from an existing kubo-system Secret (the
	// canonical source of shared credentials such as Auth0 clients).
	// +optional
	CopyFrom *VaultSeedCopyFrom `json:"copyFrom,omitempty"`
}

// GeneratedKey describes a random value to generate for a Secret key.
type GeneratedKey struct {
	// Kind of value to generate:
	//   hex    — Length random hex characters (default 32).
	//   base64 — Length random bytes, base64-encoded (default 32).
	//   uuid   — random UUID v4.
	//   bcrypt — random password, bcrypt-hashed; writes only the hash (for
	//            Trino password.db style files).
	//   tls    — self-signed RSA key pair; produces tls.key and tls.crt keys
	//            (Length ignored). Only valid with Secret type
	//            kubernetes.io/tls.
	// +kubebuilder:validation:Enum=hex;base64;uuid;bcrypt;tls
	Kind string `json:"kind"`

	// Length of the generated value (kind-dependent, see Kind).
	// +kubebuilder:validation:Minimum=8
	// +optional
	Length int `json:"length,omitempty"`
}

// VaultSeedCopyFrom copies keys from an existing Secret.
type VaultSeedCopyFrom struct {
	// Name of the source Secret.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the source Secret. Defaults to kubo-system. Use
	// acr-cache-system for canonical registry credentials.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Keys maps this Secret's key → source Secret key. When empty, ALL
	// source keys are copied and the source Secret's type is preserved.
	// +optional
	Keys map[string]string `json:"keys,omitempty"`
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
	// CertManager installs jetstack cert-manager into the operators namespace.
	// Tenant charts that need certificates (or ExternalSecret stores) can then
	// rely on it being present.
	// +optional
	CertManager bool `json:"certManager,omitempty"`
	// Postgres installs the kubegres operator. Stacks then run Postgres via
	// Postgres custom resources instead of a per-stack postgres chart.
	// +optional
	Postgres bool `json:"postgres,omitempty"`
	// AgentFW deploys the kubo-native agent firewall proxy into each tenant
	// namespace. Agents route egress through it for DLP and injection scanning.
	// +optional
	AgentFW bool `json:"agentFW,omitempty"`
	// Kubeflow installs the Kubeflow training-operator into the operators
	// namespace. Stacks can then run PyTorchJob, TFJob, and MPIJob workloads.
	// +optional
	Kubeflow bool `json:"kubeflow,omitempty"`
}

// BundleSource is an OCI artifact that carries the stack's charts.
type BundleSource struct {
	// URL is an OCI reference, including the tag or digest.
	// Example: oci://ghcr.io/org/product-bundle:0.0.8
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
// abstraction is product-agnostic: product-a, product-b, product-c or any
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

// SecretMapping copies a Secret from kubo-system into the tenant namespace.
type SecretMapping struct {
	// From is the name of the Secret in kubo-system.
	From string `json:"from"`
	// To is the name to give the Secret in the tenant namespace.
	// Defaults to From when omitted.
	// +optional
	To string `json:"to,omitempty"`
}

func init() {
	SchemeBuilder.Register(&Stack{}, &StackList{})
}
