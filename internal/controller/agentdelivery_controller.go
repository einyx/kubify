package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/sbx"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	agentDeliveryFinalizer = "platform.kubo.io/agent-delivery-sandbox"
	defaultSBXSecret       = "docker-sandboxes"
	maxDeliveryOutput      = 32 << 10
)

type SBXFactory func(token string) sbx.Service

// AgentDeliveryReconciler runs one delivery command in a Docker cloud sandbox.
// The Docker token remains in kubo-system and is never passed into the sandbox.
// +kubebuilder:rbac:groups=platform.kubo.io,resources=agentdeliveries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=agentdeliveries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=agentdeliveries/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
type AgentDeliveryReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	HTTPClient      *http.Client
	SystemNamespace string
	SecretName      string
	NewSBX          SBXFactory
}

func (r *AgentDeliveryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var delivery platformv1alpha1.AgentDelivery
	if err := r.Get(ctx, req.NamespacedName, &delivery); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	runtimeType := delivery.Spec.Sandbox.Runtime
	if runtimeType == "" {
		runtimeType = platformv1alpha1.AgentDeliveryRuntimeKubernetes
	}
	if runtimeType == platformv1alpha1.AgentDeliveryRuntimeKubernetes {
		return r.reconcileKubernetes(ctx, &delivery)
	}

	service, err := r.service(ctx)
	if err != nil {
		return r.fail(ctx, &delivery, "CredentialsUnavailable", err)
	}

	if !delivery.DeletionTimestamp.IsZero() {
		if containsString(delivery.Finalizers, agentDeliveryFinalizer) {
			if delivery.Status.SandboxName != "" {
				if err := service.Delete(ctx, delivery.Status.SandboxName); err != nil {
					return ctrl.Result{}, err
				}
			}
			delivery.Finalizers = removeString(delivery.Finalizers, agentDeliveryFinalizer)
			return ctrl.Result{}, r.Update(ctx, &delivery)
		}
		return ctrl.Result{}, nil
	}

	if delivery.Status.Phase == platformv1alpha1.AgentDeliverySucceeded || delivery.Status.Phase == platformv1alpha1.AgentDeliveryFailed {
		return ctrl.Result{}, nil
	}
	if (delivery.Spec.Sandbox.Agent == "") == (delivery.Spec.Sandbox.ImageRef == "") {
		return r.fail(ctx, &delivery, "InvalidSpec", fmt.Errorf("exactly one of spec.sandbox.agent or spec.sandbox.imageRef is required"))
	}

	if !containsString(delivery.Finalizers, agentDeliveryFinalizer) {
		delivery.Finalizers = append(delivery.Finalizers, agentDeliveryFinalizer)
		if err := r.Update(ctx, &delivery); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if delivery.Status.SandboxName == "" {
		return r.create(ctx, &delivery, service)
	}

	sandbox, err := service.Get(ctx, delivery.Status.SandboxName)
	if err != nil {
		return r.fail(ctx, &delivery, "SandboxReadFailed", err)
	}
	switch strings.ToLower(sandbox.Core.Status) {
	case "creating", "starting", "stopping":
		delivery.Status.Phase = platformv1alpha1.AgentDeliveryCreating
		delivery.Status.Message = "Waiting for Docker sandbox to become ready"
		if err := r.Status().Update(ctx, &delivery); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	case "failed", "deleted":
		return r.fail(ctx, &delivery, "SandboxFailed", fmt.Errorf("sandbox entered %s state", sandbox.Core.Status))
	case "running", "ready":
		return r.execute(ctx, &delivery, service, sandbox)
	default:
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
}

func (r *AgentDeliveryReconciler) create(ctx context.Context, delivery *platformv1alpha1.AgentDelivery, service sbx.Service) (ctrl.Result, error) {
	resources := &sbx.Resources{CPUs: delivery.Spec.Sandbox.CPUs, MemoryMiB: delivery.Spec.Sandbox.MemoryMiB}
	if resources.CPUs == nil && resources.MemoryMiB == nil {
		resources = nil
	}
	revision := delivery.Spec.Source.Revision
	if revision == "" {
		revision = "main"
	}
	sandbox, err := service.Create(ctx, sbx.CreateRequest{
		DisplayName: safeDisplayName(delivery.Namespace + "-" + delivery.Name),
		Agent:       delivery.Spec.Sandbox.Agent, ImageRef: delivery.Spec.Sandbox.ImageRef,
		PolicyIDs: delivery.Spec.Sandbox.PolicyIDs, Resources: resources,
		Environment: map[string]string{"KUBO_REPOSITORY": delivery.Spec.Source.Repository, "KUBO_REVISION": revision},
		Labels:      map[string]string{"kubo.namespace": delivery.Namespace, "kubo.agentDelivery": delivery.Name, "kubo.uid": string(delivery.UID)},
	}, string(delivery.UID)+"-"+fmt.Sprint(delivery.Generation))
	if err != nil {
		return r.fail(ctx, delivery, "SandboxCreateFailed", err)
	}
	delivery.Status.Phase = platformv1alpha1.AgentDeliveryCreating
	delivery.Status.SandboxName = sandbox.Name
	delivery.Status.Message = "Docker sandbox admitted"
	delivery.Status.ObservedGeneration = delivery.Generation
	apimeta.SetStatusCondition(&delivery.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Creating", Message: delivery.Status.Message, ObservedGeneration: delivery.Generation})
	if err := r.Status().Update(ctx, delivery); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}

func (r *AgentDeliveryReconciler) execute(ctx context.Context, delivery *platformv1alpha1.AgentDelivery, service sbx.Service, sandbox sbx.Sandbox) (ctrl.Result, error) {
	// Persist Running before exec. Since the experimental one-shot exec endpoint
	// has no idempotency key, a controller restart must not silently run the
	// agent twice. A Running delivery requires explicit operator intervention.
	if delivery.Status.Phase == platformv1alpha1.AgentDeliveryRunning {
		return ctrl.Result{}, nil
	}
	delivery.Status.Phase = platformv1alpha1.AgentDeliveryRunning
	delivery.Status.Message = "Agent command started"
	if err := r.Status().Update(ctx, delivery); err != nil {
		return ctrl.Result{}, err
	}
	revision := delivery.Spec.Source.Revision
	if revision == "" {
		revision = "main"
	}
	result, err := service.Exec(ctx, sandbox, sbx.ExecRequest{Cmd: delivery.Spec.Command, Env: map[string]string{
		"KUBO_REPOSITORY":  delivery.Spec.Source.Repository,
		"KUBO_REVISION":    revision,
		"KUBO_TASK":        delivery.Spec.Task,
		"KUBO_OUTPUT_TYPE": delivery.Spec.Output.Type,
	}})
	if err != nil {
		return r.fail(ctx, delivery, "ExecutionFailed", err)
	}
	delivery.Status.ExitCode = &result.ExitCode
	delivery.Status.Output = boundedOutput(result.Stdout, result.Stderr)
	if result.ExitCode != 0 {
		return r.fail(ctx, delivery, "CommandFailed", fmt.Errorf("agent command exited with code %d", result.ExitCode))
	}
	delivery.Status.Phase = platformv1alpha1.AgentDeliverySucceeded
	delivery.Status.Message = "Agent delivery completed"
	delivery.Status.ObservedGeneration = delivery.Generation
	apimeta.SetStatusCondition(&delivery.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Succeeded", Message: delivery.Status.Message, ObservedGeneration: delivery.Generation})
	if err := r.Status().Update(ctx, delivery); err != nil {
		return ctrl.Result{}, err
	}
	if !delivery.Spec.RetainSandbox {
		if err := service.Delete(ctx, delivery.Status.SandboxName); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *AgentDeliveryReconciler) service(ctx context.Context) (sbx.Service, error) {
	ns := r.SystemNamespace
	if ns == "" {
		ns = "kubo-system"
	}
	name := r.SecretName
	if name == "" {
		name = defaultSBXSecret
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &secret); err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return nil, fmt.Errorf("secret %s/%s has no token key", ns, name)
	}
	if r.NewSBX != nil {
		return r.NewSBX(token), nil
	}
	return sbx.New(token, r.HTTPClient), nil
}

func (r *AgentDeliveryReconciler) fail(ctx context.Context, delivery *platformv1alpha1.AgentDelivery, reason string, err error) (ctrl.Result, error) {
	delivery.Status.Phase = platformv1alpha1.AgentDeliveryFailed
	delivery.Status.Message = err.Error()
	delivery.Status.ObservedGeneration = delivery.Generation
	apimeta.SetStatusCondition(&delivery.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: err.Error(), ObservedGeneration: delivery.Generation})
	if updateErr := r.Status().Update(ctx, delivery); updateErr != nil && !apierrors.IsConflict(updateErr) {
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{}, nil
}

func (r *AgentDeliveryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.AgentDelivery{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func safeDisplayName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func boundedOutput(stdout, stderr string) string {
	out := strings.TrimSpace(stdout)
	if s := strings.TrimSpace(stderr); s != "" {
		if out != "" {
			out += "\n"
		}
		out += s
	}
	if len(out) > maxDeliveryOutput {
		out = out[:maxDeliveryOutput] + "\n[truncated]"
	}
	return out
}

func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}
func removeString(items []string, value string) []string {
	out := items[:0]
	for _, item := range items {
		if item != value {
			out = append(out, item)
		}
	}
	return out
}
