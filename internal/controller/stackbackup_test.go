package controller

import (
	"context"
	"strings"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func jobWithFailures(failed int32, limit *int32) batchv1.Job {
	return batchv1.Job{
		Spec:   batchv1.JobSpec{BackoffLimit: limit},
		Status: batchv1.JobStatus{Failed: failed},
	}
}

func backupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func TestBuildJobWiring(t *testing.T) {
	bk := &platformv1alpha1.StackBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "a-to-b", Namespace: "stack-a"},
		Spec: platformv1alpha1.StackBackupSpec{
			SourceNamespace: "stack-a",
			TargetNamespace: "stack-b",
			Include:         []string{"database", "s3"},
			Postgres: &platformv1alpha1.PostgresBackup{
				Database: "main",
				User:     "app",
			},
		},
	}
	job := (&StackBackupReconciler{}).buildJob(bk, "test-job")

	if job.Namespace != "stack-a" || job.Name != "test-job" {
		t.Errorf("job coordinates wrong: %s/%s", job.Namespace, job.Name)
	}
	if *job.Spec.BackoffLimit != 2 || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("backoff/ttl wrong: %v %v", job.Spec.BackoffLimit, job.Spec.TTLSecondsAfterFinished)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != defaultPGImage {
		t.Errorf("image = %q, want %q", c.Image, defaultPGImage)
	}
	if job.Spec.Template.Spec.ServiceAccountName != stackBackupSA {
		t.Errorf("serviceAccount = %q", job.Spec.Template.Spec.ServiceAccountName)
	}

	env := map[string]string{}
	for _, e := range c.Env {
		if e.Value != "" {
			env[e.Name] = e.Value
			continue
		}
		env[e.Name] = e.ValueFrom.SecretKeyRef.LocalObjectReference.Name + "/" + e.ValueFrom.SecretKeyRef.Key
	}
	// Access key IDs are the tenant identities (namespace names).
	if env["SRC_S3_AK"] != "stack-a" || env["DST_S3_AK"] != "stack-b" {
		t.Errorf("S3 access keys wrong: %v", env)
	}
	// Secret keys follow the storage-engine convention. DST refs point at
	// the job-local copies the reconciler makes of the target-ns secrets
	// (cross-namespace secret refs are impossible in k8s).
	if env["SRC_S3_SK"] != "storage-engine/auth-credential" || env["DST_S3_SK"] != "stackbackup-target-s3/auth-credential" {
		t.Errorf("S3 secret keys wrong: %v", env)
	}
	if env["SRC_PGPASSWORD"] != "postgres-postgresql/postgres-password" {
		t.Errorf("pg password ref wrong: %v", env["SRC_PGPASSWORD"])
	}
	if env["DST_PGPASSWORD"] != "stackbackup-target-pg/postgres-password" {
		t.Errorf("dst pg ref wrong: %v", env["DST_PGPASSWORD"])
	}

	script := c.Command[2]
	for _, want := range []string{"pg_dump", "mc mirror", "mc alias set src", "mc alias set dst", "stack-a.svc.cluster.local"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q", want)
		}
	}
}

func TestBuildJobOverrides(t *testing.T) {
	custom := "reg.io/example/backup-tools:v5"
	bk := &platformv1alpha1.StackBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns1"},
		Spec: platformv1alpha1.StackBackupSpec{
			SourceNamespace: "ns1",
			TargetNamespace: "ns2",
			Include:         []string{"s3"}, // database excluded
			Image:           custom,
			Postgres: &platformv1alpha1.PostgresBackup{
				SourcePasswordSecret: &platformv1alpha1.SecretKeyRef{Name: "pg-src", Key: "pw"},
				TargetPasswordSecret: &platformv1alpha1.SecretKeyRef{Name: "pg-dst", Key: "pw"},
			},
			S3: &platformv1alpha1.S3Backup{
				Endpoint:                "http://se.ns1:8080",
				Buckets:                 []string{"one", "two"},
				SourceCredentialsSecret: &platformv1alpha1.SecretKeyRef{Name: "creds", Key: "sk"},
			},
		},
	}
	job := (&StackBackupReconciler{}).buildJob(bk, "j")
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != custom {
		t.Errorf("custom image ignored: %q", c.Image)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if e.Value != "" {
			env[e.Name] = e.Value
		} else {
			env[e.Name] = e.ValueFrom.SecretKeyRef.LocalObjectReference.Name + "/" + e.ValueFrom.SecretKeyRef.Key
		}
	}
	if env["SRC_PGPASSWORD"] != "pg-src/pw" {
		t.Errorf("pg source override ignored: %v", env)
	}
	// DST always uses the reconciler-made target-credential copies.
	if env["DST_PGPASSWORD"] != "stackbackup-target-pg/postgres-password" {
		t.Errorf("dst pg ref wrong: %v", env)
	}
	if env["SRC_S3_SK"] != "creds/sk" {
		t.Errorf("s3 credential override ignored: %v", env["SRC_S3_SK"])
	}
	if !strings.Contains(c.Command[2], `"one two"`) {
		t.Error("buckets not passed to script")
	}
	// database excluded -> its block is gated off
	if !strings.Contains(c.Command[2], "if false; then") || !strings.Contains(c.Command[2], "if true; then") {
		t.Error("include=[s3] should enable only the s3 block")
	}
}

func TestReconcileRequiresPostgresFields(t *testing.T) {
	bk := &platformv1alpha1.StackBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "a-to-b", Namespace: "stack-a", Generation: 1},
		Spec: platformv1alpha1.StackBackupSpec{
			SourceNamespace: "stack-a",
			TargetNamespace: "stack-b",
			Include:         []string{"database"},
			// no postgres.database/user -> must fail fast, no job
		},
	}
	sch := backupScheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(bk).WithStatusSubresource(bk).Build()
	r := &StackBackupReconciler{Client: c, Scheme: sch}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: bk.Namespace, Name: bk.Name}})
	if err == nil || !strings.Contains(err.Error(), "spec.postgres.database") {
		t.Fatalf("want validation error, got %v", err)
	}
	var updated platformv1alpha1.StackBackup
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: bk.Namespace, Name: bk.Name}, &updated)
	if updated.Status.Phase != "Failed" {
		t.Errorf("phase = %q, want Failed", updated.Status.Phase)
	}
}

func TestRewriteHost(t *testing.T) {
	tests := []struct {
		endpoint, ns, want string
	}{
		{"http://storage-engine:8080", "stack-b", "http://storage-engine.stack-b.svc.cluster.local:8080"},
		{"http://storage-engine", "stack-b", "http://storage-engine.stack-b.svc.cluster.local"},
		{"https://se.internal:443/path", "ns", "https://se.internal.ns.svc.cluster.local:443/path"},
	}
	for _, tt := range tests {
		if got := rewriteHost(tt.endpoint, tt.ns); got != tt.want {
			t.Errorf("rewriteHost(%q,%q) = %q, want %q", tt.endpoint, tt.ns, got, tt.want)
		}
	}
}

func TestJobBackoffExceeded(t *testing.T) {
	limit := int32(2)
	j := jobWithFailures(2, &limit)
	if jobBackoffExceeded(&j) {
		t.Error("2 failures at limit 2 should not be exceeded")
	}
	j = jobWithFailures(3, &limit)
	if !jobBackoffExceeded(&j) {
		t.Error("3 failures at limit 2 should be exceeded")
	}
	j = jobWithFailures(7, nil)
	if !jobBackoffExceeded(&j) {
		t.Error("7 failures at default limit 6 should be exceeded")
	}
}
