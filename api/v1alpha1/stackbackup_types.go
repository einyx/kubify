/*
Copyright 2026 The Kubo Authors.
*/

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// StackBackupSpec copies Postgres databases and storage-engine S3 buckets
// from a source Stack namespace into a target Stack namespace.
type StackBackupSpec struct {
	// SourceNamespace is the namespace of the source Stack (e.g. foundation-a).
	// +kubebuilder:validation:MinLength=1
	SourceNamespace string `json:"sourceNamespace"`

	// TargetNamespace is the namespace of the destination Stack (e.g. foundation-b).
	// +kubebuilder:validation:MinLength=1
	TargetNamespace string `json:"targetNamespace"`

	// Include selects what to transfer. Defaults to [database, s3].
	// +kubebuilder:validation:items:Enum=database;s3
	// +optional
	Include []string `json:"include,omitempty"`

	// Postgres overrides defaults for the DB copy step.
	// +optional
	Postgres *PostgresBackup `json:"postgres,omitempty"`

	// S3 overrides defaults for the storage-engine copy step.
	// +optional
	S3 *S3Backup `json:"s3,omitempty"`

	// Image is the container image used for the backup job. Must have
	// pg_dump, psql, and mc (minio-client) available. Defaults to a
	// reasonable debian-based image bundling both.
	// +optional
	Image string `json:"image,omitempty"`
}

// PostgresBackup configures the DB copy step. Any field left empty uses
// the Foundation convention (service postgres-postgresql, db/user "foundation",
// password from secret postgres-postgresql key postgres-password).
type PostgresBackup struct {
	// Host is the Postgres service name inside each namespace.
	// +optional
	Host string `json:"host,omitempty"`
	// Port, defaults to 5432.
	// +optional
	Port int32 `json:"port,omitempty"`
	// Database, defaults to "foundation".
	// +optional
	Database string `json:"database,omitempty"`
	// User, defaults to "foundation".
	// +optional
	User string `json:"user,omitempty"`
	// SourcePasswordSecret is the Secret in SourceNamespace holding the password.
	// +optional
	SourcePasswordSecret *SecretKeyRef `json:"sourcePasswordSecret,omitempty"`
	// TargetPasswordSecret is the Secret in TargetNamespace holding the password.
	// +optional
	TargetPasswordSecret *SecretKeyRef `json:"targetPasswordSecret,omitempty"`
}

// S3Backup configures the storage-engine copy step.
type S3Backup struct {
	// Endpoint is the storage-engine service URL inside each namespace.
	// Defaults to http://storage-engine:8080.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// Buckets lists buckets to mirror. Empty = mirror all buckets.
	// +optional
	Buckets []string `json:"buckets,omitempty"`
	// SourceCredentialsSecret in SourceNamespace. Must contain keys
	// "access-key" and "secret-key" (or override via AccessKeyKey/SecretKeyKey).
	// +optional
	SourceCredentialsSecret *SecretKeyRef `json:"sourceCredentialsSecret,omitempty"`
	// TargetCredentialsSecret in TargetNamespace. Same shape.
	// +optional
	TargetCredentialsSecret *SecretKeyRef `json:"targetCredentialsSecret,omitempty"`
}

// SecretKeyRef points at a Secret, optionally pinning a specific key.
type SecretKeyRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Key inside the Secret. Interpretation depends on the field that
	// references it (single-value for passwords; see docs for S3 which
	// expects "access-key" and "secret-key" inside the Secret).
	// +optional
	Key string `json:"key,omitempty"`
}

// StackBackupStatus is the observed state of StackBackup.
type StackBackupStatus struct {
	// Phase: Pending | Running | Succeeded | Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// JobName is the Job created in SourceNamespace to perform the copy.
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
// +kubebuilder:resource:shortName=sbk
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceNamespace`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetNamespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// StackBackup transfers Postgres + S3 state from one Stack namespace to another.
// It spawns a one-shot Job in the source namespace that pg_dump|psql's the DB
// across and mc mirrors S3 buckets. Idempotent on the target (full overwrite).
type StackBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StackBackupSpec   `json:"spec,omitempty"`
	Status StackBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type StackBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StackBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&StackBackup{}, &StackBackupList{})
}

