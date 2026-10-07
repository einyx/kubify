package controller

import (
	"context"
	"fmt"
	"strings"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *AgentDeliveryReconciler) reconcileKubernetes(ctx context.Context, delivery *platformv1alpha1.AgentDelivery) (ctrl.Result, error) {
	if delivery.Spec.Sandbox.ImageRef == "" || delivery.Spec.Sandbox.Agent != "" {
		return r.fail(ctx, delivery, "InvalidSpec", fmt.Errorf("Kubernetes runtime requires spec.sandbox.imageRef and does not use spec.sandbox.agent"))
	}
	if len(delivery.Spec.Command) == 0 {
		return r.fail(ctx, delivery, "InvalidSpec", fmt.Errorf("spec.command is required"))
	}
	jobName := delivery.Name + "-agent"
	if len(jobName) > 63 {
		jobName = strings.TrimRight(jobName[:63], "-")
	}
	var job batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Namespace: delivery.Namespace, Name: jobName}, &job)
	if apierrors.IsNotFound(err) {
		job = *r.agentDeliveryJob(delivery, jobName)
		if r.Scheme == nil {
			return ctrl.Result{}, fmt.Errorf("agent delivery reconciler has no scheme")
		}
		if err := controllerutil.SetControllerReference(delivery, &job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &job); err != nil {
			return ctrl.Result{}, err
		}
		delivery.Status.Phase = platformv1alpha1.AgentDeliveryRunning
		delivery.Status.JobName = jobName
		delivery.Status.Message = "Kata-isolated agent Job created"
		delivery.Status.ObservedGeneration = delivery.Generation
		apimeta.SetStatusCondition(&delivery.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Running", Message: delivery.Status.Message, ObservedGeneration: delivery.Generation})
		return ctrl.Result{}, r.Status().Update(ctx, delivery)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case batchv1.JobComplete:
			delivery.Status.Phase = platformv1alpha1.AgentDeliverySucceeded
			delivery.Status.Message = "Kata-isolated agent delivery completed"
			delivery.Status.ObservedGeneration = delivery.Generation
			apimeta.SetStatusCondition(&delivery.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Succeeded", Message: delivery.Status.Message, ObservedGeneration: delivery.Generation})
			return ctrl.Result{}, r.Status().Update(ctx, delivery)
		case batchv1.JobFailed:
			message := condition.Message
			if message == "" {
				message = "agent Job failed"
			}
			return r.fail(ctx, delivery, "JobFailed", fmt.Errorf("%s", message))
		}
	}
	if delivery.Status.Phase != platformv1alpha1.AgentDeliveryRunning || delivery.Status.JobName != jobName {
		delivery.Status.Phase = platformv1alpha1.AgentDeliveryRunning
		delivery.Status.JobName = jobName
		delivery.Status.Message = "Kata-isolated agent Job is running"
		delivery.Status.ObservedGeneration = delivery.Generation
		return ctrl.Result{}, r.Status().Update(ctx, delivery)
	}
	return ctrl.Result{}, nil
}

func (r *AgentDeliveryReconciler) agentDeliveryJob(delivery *platformv1alpha1.AgentDelivery, name string) *batchv1.Job {
	runtimeClass := delivery.Spec.Sandbox.RuntimeClassName
	if runtimeClass == "" {
		runtimeClass = "kata-vm-isolation"
	}
	timeout := int64(1800)
	if delivery.Spec.Sandbox.TimeoutSeconds != nil {
		timeout = *delivery.Spec.Sandbox.TimeoutSeconds
	}
	backoff := int32(0)
	automount := false
	if delivery.Spec.Sandbox.ServiceAccountName != "" {
		automount = true
	}
	revision := delivery.Spec.Source.Revision
	if revision == "" {
		revision = "main"
	}
	envFrom := make([]corev1.EnvFromSource, 0, len(delivery.Spec.Sandbox.EnvFromSecrets))
	for _, name := range delivery.Spec.Sandbox.EnvFromSecrets {
		envFrom = append(envFrom, corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name}}})
	}
	resources := corev1.ResourceRequirements{}
	if delivery.Spec.Sandbox.CPUs != nil {
		q := resource.MustParse(fmt.Sprintf("%d", *delivery.Spec.Sandbox.CPUs))
		resources.Requests = corev1.ResourceList{corev1.ResourceCPU: q}
		resources.Limits = corev1.ResourceList{corev1.ResourceCPU: q}
	}
	if delivery.Spec.Sandbox.MemoryMiB != nil {
		q := resource.MustParse(fmt.Sprintf("%dMi", *delivery.Spec.Sandbox.MemoryMiB))
		if resources.Requests == nil {
			resources.Requests = corev1.ResourceList{}
		}
		if resources.Limits == nil {
			resources.Limits = corev1.ResourceList{}
		}
		resources.Requests[corev1.ResourceMemory] = q
		resources.Limits[corev1.ResourceMemory] = q
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: delivery.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "kubo", "platform.kubo.io/agent-delivery": delivery.Name}},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff, ActiveDeadlineSeconds: &timeout,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"platform.kubo.io/agent-delivery": delivery.Name}},
				Spec: corev1.PodSpec{
					RuntimeClassName: &runtimeClass, RestartPolicy: corev1.RestartPolicyNever,
					ServiceAccountName: delivery.Spec.Sandbox.ServiceAccountName, AutomountServiceAccountToken: &automount,
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: agentBoolPtr(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name: "agent", Image: delivery.Spec.Sandbox.ImageRef, ImagePullPolicy: corev1.PullIfNotPresent,
						Command: delivery.Spec.Command, EnvFrom: envFrom, Resources: resources,
						Env:             []corev1.EnvVar{{Name: "KUBO_REPOSITORY", Value: delivery.Spec.Source.Repository}, {Name: "KUBO_REVISION", Value: revision}, {Name: "KUBO_TASK", Value: delivery.Spec.Task}, {Name: "KUBO_OUTPUT_TYPE", Value: delivery.Spec.Output.Type}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: agentBoolPtr(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					}},
				},
			},
		},
	}
}

func agentBoolPtr(value bool) *bool { return &value }
