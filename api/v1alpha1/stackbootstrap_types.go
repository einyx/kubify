/*
Copyright 2026 The Kubo Authors.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StackBootstrapSpec runs an arbitrary bootstrap product (container image +
// command) against the Stack namespace the resource lives in. The kubo
// controller wraps it in a one-shot Job and surfaces its outcome — the
// bootstrap logic itself lives entirely in the image, so any product/script
// can bootstrap itself without touching the controller.
//
// Example: run the foundation-bootstrap product against this namespace:
//
//	apiVersion: platform.kubo.io/v1alpha1
//	kind: StackBootstrap
//	metadata:
//	  name: foundation
//	spec:
//	  image: ghcr.io/meshxdata/foundation-bootstrap:latest
//	  params:
//	    STACK_NAME: foundation
//	    TENANT: acme
//	  secrets:
//	  - name: foundation-acme-ai-secrets
type StackBootstrapSpec struct {
	// Image is the bootstrap product container image. It must be runnable
	// with no orchestration from the controller (the script owns its logic).
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Command overrides the image entrypoint. +optional
	Command []string `json:"command,omitempty"`

	// Args are passed to the entrypoint. +optional
	Args []string `json:"args,omitempty"`

	// Env are environment variables for the bootstrap container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom sources (ConfigMap/Secret) for the bootstrap container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Params are KEY=VALUE environment variables injected into the bootstrap
	// container (in addition to Env). Prefer these for plain product knobs.
	// +optional
	Params map[string]string `json:"params,omitempty"`

	// Files are inline files rendered into a ConfigMap mounted at
	// /bootstrap/files (key = file name). Useful for shipping small scripts
	// or config alongside the image without baking them in.
	// +optional
	Files map[string]string `json:"files,omitempty"`

	// Secrets are mounted read-only at /bootstrap/secrets/<name>/. Scripts
	// read credentials from there instead of taking them via env.
	// +optional
	Secrets []BootstrapSecret `json:"secrets,omitempty"`

	// ServiceAccountName for the Job. Defaults to kubo-stackbootstrap.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// Timeout is the max runtime of the bootstrap Job. Defaults to 30m.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// BackoffLimit for the Job. Defaults to 0 — bootstrap scripts are
	// typically not safe to blind-retry. +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
}

// BootstrapSecret mounts a Secret into the bootstrap container.
type BootstrapSecret struct {
	// Name of the Secret, in the same namespace as the StackBootstrap.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// MountPath overrides the default /bootstrap/secrets/<name>.
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// Optional marks the Secret as optional (Job starts without it).
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// StackBootstrapStatus is the observed state of StackBootstrap.
type StackBootstrapStatus struct {
	// Phase: Pending | Running | Succeeded | Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// JobName is the Job created in the StackBootstrap namespace.
	// +optional
	JobName string `json:"jobName,omitempty"`
	// StartedAt is when the Job began.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// CompletedAt is when the Job finished (success or failure).
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// Message is a human-readable status description or last error.
	// +optional
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sboot
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StackBootstrap runs an arbitrary bootstrap product against a Stack
// namespace as a one-shot Job.
type StackBootstrap struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StackBootstrapSpec   `json:"spec,omitempty"`
	Status StackBootstrapStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type StackBootstrapList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StackBootstrap `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StackBootstrap{}, &StackBootstrapList{})
}
