/*
Copyright 2025 The Kubo Authors.
*/

package v1alpha1

import (
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StackComponentSpec defines a single deployable component of a stack.
// Components are rendered as Helm releases by default, but the type is
// generic enough to describe any chart-based workload.
type StackComponentSpec struct {
	// Name of the component (also used as the Helm release name).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// ChartRef points to the chart backing this component.
	ChartRef ChartRef `json:"chartRef"`

	// Values is default Helm values (deep-merged: StackDefinition defaults
	// < Stack instance overrides).
	// +optional
	Values apiextensionsv1.JSON `json:"values,omitempty"`

	// DependsOn lists component names that must be Ready before this one.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

	// Scope is Namespaced (default) or Cluster. Cluster components are
	// installed once for the whole cluster (vault-operator, spark-operator)
	// and are not repeated per Stack. Each Stack still owns its own
	// namespaced Vault and Spark jobs, which the shared operator watches.
	// +kubebuilder:validation:Enum=Namespaced;Cluster
	// +kubebuilder:default=Namespaced
	// +optional
	Scope ComponentScope `json:"scope,omitempty"`

	// ChartPullSecretRef references a Secret with registry credentials
	// (imagePullSecret/dockerconfigjson format) used to pull the chart.
	// Applied to the Flux HelmRepository in Flux mode and to the Helm
	// registry client in Direct mode.
	// +optional
	ChartPullSecretRef *v1.LocalObjectReference `json:"chartPullSecretRef,omitempty"`

	// RegistryAuth selects how the registry is authenticated when no
	// static secret is used. Currently supports "azure" (workload identity
	// via Flux source-controller) — e.g. ACR on AKS without a pull secret.
	// +kubebuilder:validation:Enum=secret;azure
	// +optional
	RegistryAuth string `json:"registryAuth,omitempty"`
}

// ComponentScope selects where a component is installed.
type ComponentScope string

const (
	// ComponentScopeNamespaced installs the release in the Stack namespace.
	ComponentScopeNamespaced ComponentScope = "Namespaced"
	// ComponentScopeCluster installs the release once in the operator namespace.
	ComponentScopeCluster ComponentScope = "Cluster"
)

// ChartRef locates a Helm chart.
type ChartRef struct {
	// RepoURL is the Helm repository URL. Optional when the chart is
	// resolved from a Bundle or ExtraBundles entry.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`

	// ChartName is the chart name within the repository.
	// +kubebuilder:validation:MinLength=1
	ChartName string `json:"chartName"`

	// ChartVersion pins the chart version. Empty = latest available.
	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`
}

// StackDefinitionSpec is the pluggable description of a stack product
// (e.g. product, dai, product-ai, or anything else).
type StackDefinitionSpec struct {
	// Title is a human readable name.
	Title string `json:"title"`

	// Description of what this stack deploys.
	// +optional
	Description string `json:"description,omitempty"`

	// Components is the ordered set of components to deploy.
	// +kubebuilder:validation:MinItems=1
	Components []StackComponentSpec `json:"components"`

	// ValuesSchema optionally documents/validates overridable values.
	// +optional
	ValuesSchema apiextensionsv1.JSON `json:"valuesSchema,omitempty"`
}

// StackDefinitionStatus defines the observed state of StackDefinition.
type StackDefinitionStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=stackdef
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Components",type=integer,JSONPath=`.spec.components`

// StackDefinition is the cluster-level, pluggable blueprint of a product
// stack. Any product can be plugged in by authoring one of these — the
// Stack controller is generic and knows nothing about specific products.
type StackDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StackDefinitionSpec   `json:"spec,omitempty"`
	Status StackDefinitionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StackDefinitionList contains a list of StackDefinition.
type StackDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StackDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StackDefinition{}, &StackDefinitionList{})
}
