package controller

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/sbx"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeSBX struct {
	sandbox  sbx.Sandbox
	created  int
	executed int
	deleted  int
}

func TestAgentDeliveryCreatesKataJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	delivery := &platformv1alpha1.AgentDelivery{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-run", Namespace: "default", UID: types.UID("kata-uid")},
		Spec: platformv1alpha1.AgentDeliverySpec{
			Source:  platformv1alpha1.AgentDeliverySource{Repository: "https://github.com/acme/app.git"},
			Task:    "run tests",
			Sandbox: platformv1alpha1.AgentDeliverySandbox{ImageRef: "ghcr.io/acme/agent:v1"},
			Command: []string{"agent-deliver"},
		},
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(delivery, &batchv1.Job{}).WithObjects(delivery).Build()
	r := &AgentDeliveryReconciler{Client: k8s, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: delivery.Namespace, Name: delivery.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := k8s.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "kata-run-agent"}, &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.Template.Spec.RuntimeClassName == nil || *job.Spec.Template.Spec.RuntimeClassName != "kata-vm-isolation" {
		t.Fatalf("runtime class = %v", job.Spec.Template.Spec.RuntimeClassName)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("service account token should not be mounted by default")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "ghcr.io/acme/agent:v1" || container.Command[0] != "agent-deliver" {
		t.Fatalf("container = %#v", container)
	}
}

func (f *fakeSBX) Create(context.Context, sbx.CreateRequest, string) (sbx.Sandbox, error) {
	f.created++
	return f.sandbox, nil
}

func (f *fakeSBX) Get(context.Context, string) (sbx.Sandbox, error) { return f.sandbox, nil }

func (f *fakeSBX) Exec(context.Context, sbx.Sandbox, sbx.ExecRequest) (sbx.ExecResult, error) {
	f.executed++
	return sbx.ExecResult{ExitCode: 0, Stdout: "pull request opened\n"}, nil
}

func (f *fakeSBX) Delete(context.Context, string) error { f.deleted++; return nil }

func TestAgentDeliveryReconcileToSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	delivery := &platformv1alpha1.AgentDelivery{
		ObjectMeta: metav1.ObjectMeta{Name: "fix-184", Namespace: "default", UID: types.UID("delivery-uid")},
		Spec: platformv1alpha1.AgentDeliverySpec{
			Source:  platformv1alpha1.AgentDeliverySource{Repository: "https://github.com/acme/app.git", Revision: "main"},
			Task:    "fix issue 184",
			Sandbox: platformv1alpha1.AgentDeliverySandbox{Runtime: platformv1alpha1.AgentDeliveryRuntimeDockerCloud, Agent: "claude"},
			Output:  platformv1alpha1.AgentDeliveryOutput{Type: "PullRequest"},
			Command: []string{"kubo-agent-deliver"},
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: defaultSBXSecret, Namespace: "kubo-system"}, Data: map[string][]byte{"token": []byte("test-token")}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(delivery).WithObjects(delivery, secret).Build()
	docker := &fakeSBX{sandbox: sbx.Sandbox{Name: "sandboxes/test", Core: sbx.SandboxCore{Status: "running", Endpoint: sbx.Endpoint{URI: "https://sandbox.invalid"}}}}
	r := &AgentDeliveryReconciler{Client: k8s, NewSBX: func(token string) sbx.Service {
		if token != "test-token" {
			t.Fatalf("token = %q", token)
		}
		return docker
	}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: delivery.Namespace, Name: delivery.Name}}

	// Add finalizer, create sandbox, then execute the delivery.
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}

	var got platformv1alpha1.AgentDelivery
	if err := k8s.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.AgentDeliverySucceeded {
		t.Fatalf("phase = %q, message = %q", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Output != "pull request opened" {
		t.Fatalf("output = %q", got.Status.Output)
	}
	if docker.created != 1 || docker.executed != 1 || docker.deleted != 1 {
		t.Fatalf("calls: create=%d exec=%d delete=%d", docker.created, docker.executed, docker.deleted)
	}

	// Terminal status is stable and must not execute the agent again.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if docker.executed != 1 {
		t.Fatalf("agent executed %d times", docker.executed)
	}
}
