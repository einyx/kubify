/*
Copyright 2026 The Kubo Authors.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
	stackBackupSA   = "kubo-stackbackup"
	defaultPGImage  = "meshxregistry.azurecr.io/kubo/stack-backup:pg17" // pg_dump/psql v17 + mc bundled (runtime downloads break behind proxies)
	defaultPGHost   = "postgres-postgresql"
	defaultPGSecret = "postgres-postgresql"
	defaultPGPwdKey = "postgres-password"
	defaultS3URL    = "http://storage-engine:8080"
	defaultS3Secret = "storage-engine"
	// Storage-engine convention: the chart stores only the secret access key
	// (key auth-credential); the access key ID is the tenant identity, which
	// equals the namespace name.
	defaultS3SkKey = "auth-credential"
)

// StackBackupReconciler reconciles StackBackup objects by spawning a Job
// in the source namespace that copies Postgres + S3 state to the target
// namespace.
type StackBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create

func (r *StackBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var bk platformv1alpha1.StackBackup
	if err := r.Get(ctx, req.NamespacedName, &bk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if bk.Status.Phase == "Succeeded" || bk.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}

	// Database copy needs explicit credentials context; there is no generic
	// default database/user that works across products.
	include := bk.Spec.Include
	if len(include) == 0 {
		include = []string{"database", "s3"}
	}
	if contains(include, "database") {
		pg := bk.Spec.Postgres
		if pg == nil || pg.Database == "" || pg.User == "" {
			return r.fail(ctx, &bk, "spec.postgres.database and spec.postgres.user are required for database backups")
		}
	}

	jobName := bk.Status.JobName
	if jobName == "" {
		// Deterministic per generation: concurrent reconciles Get the same
		// Job instead of racing out timestamped duplicates (seen live: two
		// identical backups running at once).
		jobName = fmt.Sprintf("stackbackup-%s-g%d", bk.Name, bk.Generation)
	}

	// Ensure ServiceAccount in source namespace (no special perms, just an identity).
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: stackBackupSA, Namespace: bk.Spec.SourceNamespace}}
	if err := r.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
		return r.fail(ctx, &bk, "create SA: "+err.Error())
	}

	// Target-side credentials live in the TARGET namespace; the Job runs in
	// the SOURCE namespace and cannot reference them (k8s Secrets are
	// namespaced). Copy the referenced values into a job-local Secret here,
	// refreshed on every reconcile so rotation propagates.
	pgSpec := bk.Spec.Postgres
	if pgSpec == nil {
		pgSpec = &platformv1alpha1.PostgresBackup{}
	}
	restoreSrcName, restoreSrcKey := "postgres-postgresql", "postgres-password"
	if pgSpec.RestorePasswordSecret != nil {
		restoreSrcName = pgSpec.RestorePasswordSecret.Name
		restoreSrcKey = pgSpec.RestorePasswordSecret.Key
	}
	targetSecrets := []struct{ localName, srcName, key string }{
		{"stackbackup-target-pg", "postgres-postgresql", "postgres-password"},
		{"stackbackup-target-s3", "storage-engine", "auth-credential"},
		{"stackbackup-target-restore-pg", restoreSrcName, restoreSrcKey},
	}
	for _, ts := range targetSecrets {
		var src corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: bk.Spec.TargetNamespace, Name: ts.srcName}, &src); err != nil {
			if errors.IsNotFound(err) {
				return r.fail(ctx, &bk, fmt.Sprintf("target secret %s/%s not found", bk.Spec.TargetNamespace, ts.srcName))
			}
			return r.fail(ctx, &bk, "get target secret: "+err.Error())
		}
		local := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ts.localName, Namespace: bk.Spec.SourceNamespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "kubo"}},
			Type: corev1.SecretTypeOpaque,
			Data: src.Data,
		}
		existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ts.localName, Namespace: bk.Spec.SourceNamespace}}
		err := r.Get(ctx, client.ObjectKeyFromObject(existing), existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, local); err != nil && !errors.IsAlreadyExists(err) {
				return r.fail(ctx, &bk, "create target-cred copy: "+err.Error())
			}
		} else if err == nil {
			existing.Data = local.Data
			if err := r.Update(ctx, existing); err != nil {
				return r.fail(ctx, &bk, "update target-cred copy: "+err.Error())
			}
		} else {
			return r.fail(ctx, &bk, "get target-cred copy: "+err.Error())
		}
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bk.Spec.SourceNamespace, Name: jobName}, job)
	if errors.IsNotFound(err) {
		job = r.buildJob(&bk, jobName)
		if err := controllerutil.SetControllerReference(&bk, job, r.Scheme); err != nil {
			return r.fail(ctx, &bk, "owner ref: "+err.Error())
		}
		if err := r.Create(ctx, job); err != nil {
			return r.fail(ctx, &bk, "create job: "+err.Error())
		}
		log.Info("stackbackup job created", "job", jobName)
		bk.Status.Phase = "Running"
		bk.Status.JobName = jobName
		now := metav1.Now()
		bk.Status.StartedAt = &now
		bk.Status.ObservedGeneration = bk.Generation
		return ctrl.Result{RequeueAfter: 15 * time.Second}, r.Status().Update(ctx, &bk)
	}
	if err != nil {
		return r.fail(ctx, &bk, "get job: "+err.Error())
	}

	// Reflect job state.
	switch {
	case job.Status.Succeeded > 0:
		now := metav1.Now()
		bk.Status.Phase = "Succeeded"
		bk.Status.CompletedAt = &now
		bk.Status.Message = "backup completed"
		return ctrl.Result{}, r.Status().Update(ctx, &bk)
	case job.Status.Failed > 0 && jobBackoffExceeded(job):
		now := metav1.Now()
		bk.Status.Phase = "Failed"
		bk.Status.CompletedAt = &now
		bk.Status.Message = "job failed; see Job logs: kubectl -n " + bk.Spec.SourceNamespace + " logs job/" + jobName
		return ctrl.Result{}, r.Status().Update(ctx, &bk)
	}
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

func jobBackoffExceeded(j *batchv1.Job) bool {
	limit := int32(6)
	if j.Spec.BackoffLimit != nil {
		limit = *j.Spec.BackoffLimit
	}
	return j.Status.Failed > limit
}

func (r *StackBackupReconciler) fail(ctx context.Context, bk *platformv1alpha1.StackBackup, msg string) (ctrl.Result, error) {
	bk.Status.Phase = "Failed"
	bk.Status.Message = msg
	now := metav1.Now()
	bk.Status.CompletedAt = &now
	_ = r.Status().Update(ctx, bk)
	return ctrl.Result{}, fmt.Errorf("%s", msg)
}

func (r *StackBackupReconciler) buildJob(bk *platformv1alpha1.StackBackup, name string) *batchv1.Job {
	image := bk.Spec.Image
	if image == "" {
		image = defaultPGImage
	}

	pg := bk.Spec.Postgres
	if pg == nil {
		pg = &platformv1alpha1.PostgresBackup{}
	}
	host := firstNonEmpty(pg.Host, defaultPGHost)
	port := int32(5432)
	if pg.Port != 0 {
		port = pg.Port
	}
	db := pg.Database
	user := pg.User
	srcPgSec, srcPgKey := secretRef(pg.SourcePasswordSecret, defaultPGSecret, defaultPGPwdKey)
	dstPgSec, dstPgKey := secretRef(pg.TargetPasswordSecret, defaultPGSecret, defaultPGPwdKey)
	restoreUser := firstNonEmpty(pg.RestoreUser, pg.User)
	restoreKey := defaultPGPwdKey
	if pg.RestorePasswordSecret != nil && pg.RestorePasswordSecret.Key != "" {
		restoreKey = pg.RestorePasswordSecret.Key
	}
	// RESTORE_PGPASSWORD comes from the reconciler's cross-namespace copy.
	restoreSec := "stackbackup-target-restore-pg"
// (overridden below: target-ns values are copied to the source ns at create-time)

	s3 := bk.Spec.S3
	if s3 == nil {
		s3 = &platformv1alpha1.S3Backup{}
	}
	s3Endpoint := firstNonEmpty(s3.Endpoint, defaultS3URL)
	srcS3Sec, srcS3Key := secretRef(s3.SourceCredentialsSecret, defaultS3Secret, "")
	dstS3Sec, dstS3Key := secretRef(s3.TargetCredentialsSecret, defaultS3Secret, "")
	srcS3Key = firstNonEmpty(srcS3Key, defaultS3SkKey)
	dstS3Key = firstNonEmpty(dstS3Key, defaultS3SkKey)

	// The Job runs in the source namespace; target-side credentials were
	// copied there by the reconciler into fixed-name Secrets. Always use
	// those copies (rotation-safe: reconciler refreshes them).
	dstPgSec, dstPgKey = "stackbackup-target-pg", "postgres-password"
	dstS3Sec, dstS3Key = "stackbackup-target-s3", "auth-credential"

	include := bk.Spec.Include
	if len(include) == 0 {
		include = []string{"database", "s3"}
	}
	doDB := contains(include, "database")
	doS3 := contains(include, "s3")

	// Cross-namespace service DNS: <svc>.<ns>.svc.cluster.local
	srcDBHost := fmt.Sprintf("%s.%s.svc.cluster.local", host, bk.Spec.SourceNamespace)
	dstDBHost := fmt.Sprintf("%s.%s.svc.cluster.local", host, bk.Spec.TargetNamespace)
	srcS3Host := rewriteHost(s3Endpoint, bk.Spec.SourceNamespace)
	dstS3Host := rewriteHost(s3Endpoint, bk.Spec.TargetNamespace)

	// ponytail: inline bash script. If this grows beyond ~40 lines put it in a ConfigMap.
	buckets := strings.Join(s3.Buckets, " ")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

if %t; then
  echo "[db] reset target: drop all non-system schemas (mirror restore)"
  PGPASSWORD="$RESTORE_PGPASSWORD" psql -h %s -p %d -U %s -d %s -Atc \
    "SELECT format('DROP SCHEMA IF EXISTS %%I CASCADE', nspname) FROM pg_namespace WHERE nspname <> 'public' AND nspname NOT LIKE 'pg\\_%%' AND nspname <> 'information_schema'" \
    | PGPASSWORD="$RESTORE_PGPASSWORD" psql -h %s -p %d -U %s -d %s
  PGPASSWORD="$RESTORE_PGPASSWORD" psql -h %s -p %d -U %s -d %s \
    -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO public;'
  echo "[db] pg_dump %s -> %s"
  PGPASSWORD="$SRC_PGPASSWORD" pg_dump -h %s -p %d -U %s -d %s --no-owner \
    | PGPASSWORD="$RESTORE_PGPASSWORD" psql -h %s -p %d -U %s -d %s -v ON_ERROR_STOP=0
fi

if %t; then
  echo "[s3] mirror %s -> %s"
  if ! command -v mc >/dev/null 2>&1; then
    apk add --no-cache curl ca-certificates >/dev/null 2>&1 || true
    curl -sSL https://github.com/minio/mc/releases/download/RELEASE.2025-08-13T08-35-41Z/mc.linux-amd64.RELEASE.2025-08-13T08-35-41Z -o /usr/local/bin/mc
    chmod +x /usr/local/bin/mc
  fi
  mc alias set src %s "$SRC_S3_AK" "$SRC_S3_SK"
  mc alias set dst %s "$DST_S3_AK" "$DST_S3_SK"
  buckets="%s"
  if [ -z "$buckets" ]; then
    buckets="$(mc ls src | awk '{print $NF}' | tr -d '/')"
  fi
  for b in $buckets; do
    mc mb --ignore-existing "dst/$b"
    mc mirror --overwrite --remove "src/$b" "dst/$b"
  done
fi
echo "done."
`,
		doDB, dstDBHost, port, restoreUser, db,
		dstDBHost, port, restoreUser, db,
		dstDBHost, port, restoreUser, db,
		srcDBHost, dstDBHost,
		srcDBHost, port, user, db,
		dstDBHost, port, restoreUser, db,
		doS3, srcS3Host, dstS3Host,
		srcS3Host, dstS3Host, buckets,
	)

	backoff := int32(2)
	ttl := int32(3600) // ponytail: keep failed jobs 1h for debugging, GC after
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: bk.Spec.SourceNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kubo",
				"platform.kubo.io/stackbackup": bk.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: stackBackupSA,
					RestartPolicy:      corev1.RestartPolicyNever,
					ImagePullSecrets: []corev1.LocalObjectReference{{Name: "acr-pull-secret"}},
					Containers: []corev1.Container{{
						Name:    "backup",
						Image:   image,
						Command: []string{"bash", "-c", script},
						Env: []corev1.EnvVar{
							envFromSecret("SRC_PGPASSWORD", srcPgSec, srcPgKey),
							envFromSecret("DST_PGPASSWORD", dstPgSec, dstPgKey),
							envFromSecret("RESTORE_PGPASSWORD", restoreSec, restoreKey),
							// Access key ID = tenant identity = namespace name.
							corev1.EnvVar{Name: "SRC_S3_AK", Value: bk.Spec.SourceNamespace},
							corev1.EnvVar{Name: "DST_S3_AK", Value: bk.Spec.TargetNamespace},
							envFromSecret("SRC_S3_SK", srcS3Sec, srcS3Key),
							envFromSecret("DST_S3_SK", dstS3Sec, dstS3Key),
						},
					}},
				},
			},
		},
	}
}

func envFromSecret(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
				Optional:             boolPtr(true),
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

func secretRef(ref *platformv1alpha1.SecretKeyRef, defName, defKey string) (string, string) {
	if ref == nil {
		return defName, defKey
	}
	name := ref.Name
	key := ref.Key
	if name == "" {
		name = defName
	}
	if key == "" {
		key = defKey
	}
	return name, key
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// rewriteHost swaps the hostname of a URL like http://storage-engine:8080
// to the cross-namespace FQDN http://storage-engine.<ns>.svc.cluster.local:8080.
func rewriteHost(endpoint, ns string) string {
	// trivial parse — fine for http(s)://host[:port][/path]
	schemeEnd := strings.Index(endpoint, "://")
	if schemeEnd < 0 {
		return endpoint
	}
	rest := endpoint[schemeEnd+3:]
	hostEnd := len(rest)
	for i, c := range rest {
		if c == ':' || c == '/' {
			hostEnd = i
			break
		}
	}
	host := rest[:hostEnd]
	tail := rest[hostEnd:]
	return endpoint[:schemeEnd+3] + host + "." + ns + ".svc.cluster.local" + tail
}

func (r *StackBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.StackBackup{}).
		Owns(&batchv1.Job{}).
		Named("stackbackup").
		Complete(r)
}
