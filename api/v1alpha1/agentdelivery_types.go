package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type AgentDeliveryPhase string

type AgentDeliveryRuntime string

const (
	AgentDeliveryPending   AgentDeliveryPhase = "Pending"
	AgentDeliveryCreating  AgentDeliveryPhase = "Creating"
	AgentDeliveryRunning   AgentDeliveryPhase = "Running"
	AgentDeliverySucceeded AgentDeliveryPhase = "Succeeded"
	AgentDeliveryFailed    AgentDeliveryPhase = "Failed"
)

const (
	AgentDeliveryRuntimeKubernetes  AgentDeliveryRuntime = "Kubernetes"
	AgentDeliveryRuntimeDockerCloud AgentDeliveryRuntime = "DockerCloud"
)

type AgentDeliverySource struct {
	// Repository is the HTTPS Git repository cloned by the sandbox command.
	// +kubebuilder:validation:Pattern=`^https://`
	Repository string `json:"repository"`
	// Revision is a branch, tag, or commit. Defaults to main.
	// +optional
	Revision string `json:"revision,omitempty"`
}

type AgentDeliverySandbox struct {
	// Runtime selects the execution backend. Defaults to Kubernetes.
	// +kubebuilder:validation:Enum=Kubernetes;DockerCloud
	// +kubebuilder:default=Kubernetes
	// +optional
	Runtime AgentDeliveryRuntime `json:"runtime,omitempty"`
	// Agent selects a Docker Sandboxes agent profile.
	// +optional
	Agent string `json:"agent,omitempty"`
	// ImageRef selects an external OCI image. Mutually exclusive with Agent.
	// +optional
	ImageRef string `json:"imageRef,omitempty"`
	// PolicyIDs names Docker organization policies applied before startup.
	// +optional
	PolicyIDs []string `json:"policyIDs,omitempty"`
	// CPUs requests a cloud compute size.
	// +optional
	// +kubebuilder:validation:Minimum=1
	CPUs *int32 `json:"cpus,omitempty"`
	// MemoryMiB requests sandbox memory.
	// +optional
	// +kubebuilder:validation:Minimum=512
	MemoryMiB *int64 `json:"memoryMiB,omitempty"`
	// RuntimeClassName selects the Kubernetes isolation runtime. Defaults to
	// kata-vm-isolation for Kubernetes deliveries.
	// +optional
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	// ServiceAccountName is the identity used by the agent Job.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// EnvFromSecrets exposes explicitly selected Kubernetes Secrets to the
	// agent container. Prefer workload identity where supported.
	// +optional
	EnvFromSecrets []string `json:"envFromSecrets,omitempty"`
	// TimeoutSeconds limits a Kubernetes Job. Defaults to 1800 seconds.
	// +optional
	// +kubebuilder:validation:Minimum=60
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

type AgentDeliveryOutput struct {
	// Type describes the expected agent result.
	// +kubebuilder:validation:Enum=PullRequest;Bundle
	// +kubebuilder:default=PullRequest
	Type string `json:"type,omitempty"`
}

// AgentDeliverySpec describes one isolated agent delivery run.
type AgentDeliverySpec struct {
	Source AgentDeliverySource `json:"source"`
	// Task is the instruction supplied to the sandboxed agent.
	// +kubebuilder:validation:MinLength=1
	Task    string               `json:"task"`
	Sandbox AgentDeliverySandbox `json:"sandbox,omitempty"`
	Output  AgentDeliveryOutput  `json:"output,omitempty"`
	// Command is an argv vector executed once after the sandbox becomes ready.
	// The selected agent image or kit must provide this entrypoint. Kubo adds
	// KUBO_REPOSITORY, KUBO_REVISION, KUBO_TASK, and KUBO_OUTPUT_TYPE.
	// +kubebuilder:validation:MinItems=1
	Command []string `json:"command"`
	// RetainSandbox keeps the sandbox after a successful delivery for debugging.
	// Failed sandboxes are always retained until the AgentDelivery is deleted.
	// +optional
	RetainSandbox bool `json:"retainSandbox,omitempty"`
}

type AgentDeliveryStatus struct {
	// +optional
	Phase AgentDeliveryPhase `json:"phase,omitempty"`
	// SandboxName is Docker's immutable resource name (sandboxes/<id>).
	// +optional
	SandboxName string `json:"sandboxName,omitempty"`
	// JobName is the Kubernetes Job used by the Kata backend.
	// +optional
	JobName string `json:"jobName,omitempty"`
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Output contains bounded command output intended for diagnostics.
	// +optional
	Output string `json:"output,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=adelivery
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Sandbox",type=string,JSONPath=`.status.sandboxName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentDelivery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentDeliverySpec   `json:"spec,omitempty"`
	Status            AgentDeliveryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type AgentDeliveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentDelivery `json:"items"`
}

func init() { SchemeBuilder.Register(&AgentDelivery{}, &AgentDeliveryList{}) }
