/*
Copyright 2026 The Kubo Authors.
*/

package controller

import (
	"context"
	"fmt"
	"sort"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

const (
	stackBootstrapSA     = "kubo-stackbootstrap"
	stackBootstrapFiles  = "stackbootstrap-files"
	stackBootstrapMount  = "/bootstrap/files"
	stackBootstrapSecret = "/bootstrap/secrets"
	defaultBootstrapTO   = 30 * time.Minute
)

// StackBootstrapReconciler reconciles StackBootstrap objects by running the
// requested bootstrap product as a one-shot Job in the same namespace. The
// product logic lives in the image; the controller only orchestrates.
type StackBootstrapReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackbootstraps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackbootstraps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create

func (r *StackBootstrapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var sb platformv1alpha1.StackBootstrap
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A generation change invalidates the previous run: re-run against the
	// new spec (the old Job is orphaned — its logs remain for debugging).
	if sb.Status.Phase == "Succeeded" || sb.Status.Phase == "Failed" {
		if sb.Status.ObservedGeneration >= sb.Generation {
			return ctrl.Result{}, nil
		}
		log.Info("spec changed; re-running bootstrap", "from", sb.Status.Phase)
		sb.Status.Phase = ""
		sb.Status.JobName = ""
		sb.Status.StartedAt = nil
		sb.Status.CompletedAt = nil
	}

	jobName := sb.Status.JobName
	if jobName == "" {
		// Deterministic per generation: concurrent reconciles (SA/ConfigMap
		// creates force a requeue) Get the same Job instead of minting
		// duplicates, and a spec change naturally produces a fresh name.
		jobName = fmt.Sprintf("stackbootstrap-%s-g%d", sb.Name, sb.Generation)
	}

	// Identity for the Job (no special perms — products use their own creds).
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      saOrDefault(sb.Spec.ServiceAccountName),
		Namespace: sb.Namespace,
	}}
	if err := r.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
		return r.fail(ctx, &sb, "create SA: "+err.Error())
	}

	// Inline files → ConfigMap (only when the spec ships any).
	if len(sb.Spec.Files) > 0 {
		cm := filesConfigMap(&sb)
		if err := controllerutil.SetControllerReference(&sb, cm, r.Scheme); err != nil {
			return r.fail(ctx, &sb, "owner ref: "+err.Error())
		}
		existing := &corev1.ConfigMap{}
		err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: cm.Name}, existing)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, cm); err != nil {
				return r.fail(ctx, &sb, "create files configmap: "+err.Error())
			}
		} else if err == nil && cm.Data["generation"] != existing.Data["generation"] {
			existing.Data = cm.Data
			if err := r.Update(ctx, existing); err != nil {
				return r.fail(ctx, &sb, "update files configmap: "+err.Error())
			}
		} else if err != nil {
			return r.fail(ctx, &sb, "get files configmap: "+err.Error())
		}
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: jobName}, job)
	if apierrors.IsNotFound(err) {
		job = r.buildJob(&sb, jobName)
		if err := controllerutil.SetControllerReference(&sb, job, r.Scheme); err != nil {
			return r.fail(ctx, &sb, "owner ref: "+err.Error())
		}
		if err := r.Create(ctx, job); err != nil {
			return r.fail(ctx, &sb, "create job: "+err.Error())
		}
		log.Info("stackbootstrap job created", "job", jobName, "image", sb.Spec.Image)
		sb.Status.Phase = "Running"
		sb.Status.JobName = jobName
		now := metav1.Now()
		sb.Status.StartedAt = &now
		sb.Status.ObservedGeneration = sb.Generation
		return ctrl.Result{RequeueAfter: 15 * time.Second}, r.Status().Update(ctx, &sb)
	}
	if err != nil {
		return r.fail(ctx, &sb, "get job: "+err.Error())
	}

	// Reflect job state.
	switch {
	case job.Status.Succeeded > 0:
		now := metav1.Now()
		sb.Status.Phase = "Succeeded"
		sb.Status.CompletedAt = &now
		sb.Status.Message = "bootstrap completed"
		return ctrl.Result{}, r.Status().Update(ctx, &sb)
	case job.Status.Failed > 0 && jobBackoffExceeded(job):
		now := metav1.Now()
		sb.Status.Phase = "Failed"
		sb.Status.CompletedAt = &now
		sb.Status.Message = "bootstrap failed; logs: kubectl -n " + sb.Namespace +
			" logs job/" + jobName
		return ctrl.Result{}, r.Status().Update(ctx, &sb)
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func saOrDefault(sa string) string {
	if sa == "" {
		return stackBootstrapSA
	}
	return sa
}

func (r *StackBootstrapReconciler) fail(ctx context.Context, sb *platformv1alpha1.StackBootstrap, msg string) (ctrl.Result, error) {
	sb.Status.Phase = "Failed"
	sb.Status.Message = msg
	now := metav1.Now()
	sb.Status.CompletedAt = &now
	_ = r.Status().Update(ctx, sb)
	return ctrl.Result{}, fmt.Errorf("%s", msg)
}

// filesConfigMap renders spec.files (+ a generation marker for drift) into
// a ConfigMap owned by the StackBootstrap.
func filesConfigMap(sb *platformv1alpha1.StackBootstrap) *corev1.ConfigMap {
	data := map[string]string{}
	for k, v := range sb.Spec.Files {
		data[k] = v
	}
	data["generation"] = fmt.Sprintf("%d", sb.Generation)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", stackBootstrapFiles, sb.Name),
			Namespace: sb.Namespace,
		},
		Data: data,
	}
}

func (r *StackBootstrapReconciler) buildJob(sb *platformv1alpha1.StackBootstrap, name string) *batchv1.Job {
	timeout := defaultBootstrapTO
	if sb.Spec.Timeout != nil {
		timeout = sb.Spec.Timeout.Duration
	}
	backoff := int32(0)
	if sb.Spec.BackoffLimit != nil {
		backoff = *sb.Spec.BackoffLimit
	}

	env := []corev1.EnvVar{}
	env = append(env, sb.Spec.Env...)
	// Params sorted for deterministic jobs (same spec → identical pod template).
	keys := make([]string, 0, len(sb.Spec.Params))
	for k := range sb.Spec.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := sb.Spec.Params[k]
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}

	volumes := []corev1.Volume{}
	mounts := []corev1.VolumeMount{}
	if len(sb.Spec.Files) > 0 {
		cmName := fmt.Sprintf("%s-%s", stackBootstrapFiles, sb.Name)
		volumes = append(volumes, corev1.Volume{
			Name: "files",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "files", MountPath: stackBootstrapMount, ReadOnly: true})
	}
	for _, s := range sb.Spec.Secrets {
		mp := s.MountPath
		if mp == "" {
			mp = stackBootstrapSecret + "/" + s.Name
		}
		volumes = append(volumes, corev1.Volume{
			Name: "secret-" + s.Name,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: s.Name,
					Optional:   &s.Optional,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: "secret-" + s.Name, MountPath: mp, ReadOnly: true,
		})
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyNever,
		ServiceAccountName: saOrDefault(sb.Spec.ServiceAccountName),
		Containers: []corev1.Container{{
			Name:         "bootstrap",
			Image:        sb.Spec.Image,
			Command:      sb.Spec.Command,
			Args:         sb.Spec.Args,
			Env:          env,
			EnvFrom:      sb.Spec.EnvFrom,
			VolumeMounts: mounts,
		}},
		Volumes: volumes,
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sb.Namespace},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   durationPtr(timeout),
			TTLSecondsAfterFinished: ttlPtr(int32(7 * 24 * time.Hour / time.Second)),
			Template:                corev1.PodTemplateSpec{Spec: podSpec},
		},
	}
}

func durationPtr(d time.Duration) *int64 {
	v := int64(d.Seconds())
	return &v
}

func ttlPtr(s int32) *int32 {
	return &s
}

func (r *StackBootstrapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.StackBootstrap{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
