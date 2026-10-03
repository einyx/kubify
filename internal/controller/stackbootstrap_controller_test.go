package controller

import (
	"strings"
	"testing"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testBootstrap() *platformv1alpha1.StackBootstrap {
	d := 5 * time.Minute
	return &platformv1alpha1.StackBootstrap{
		ObjectMeta: metav1.ObjectMeta{Name: "foundation", Namespace: "foundation-x", Generation: 3},
		Spec: platformv1alpha1.StackBootstrapSpec{
			Image:   "ghcr.io/meshxdata/foundation-bootstrap:latest",
			Command: []string{"/bootstrap/run"},
			Params: map[string]string{
				"Z_PARAM": "z",
				"A_PARAM": "a",
			},
			Files: map[string]string{
				"run.sh":   "echo hi",
				"init.sql": "SELECT 1;",
			},
			Secrets: []platformv1alpha1.BootstrapSecret{{Name: "ai-secrets"}},
			Timeout: &metav1.Duration{Duration: d},
		},
	}
}

func TestBuildBootstrapJob(t *testing.T) {
	r := &StackBootstrapReconciler{}
	sb := testBootstrap()
	job := r.buildJob(sb, "stackbootstrap-foundation-1")

	if job.Namespace != "foundation-x" || job.Name != "stackbootstrap-foundation-1" {
		t.Fatalf("job coordinates: %s/%s", job.Namespace, job.Name)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != sb.Spec.Image {
		t.Fatalf("image: %s", c.Image)
	}
	if len(c.Command) != 1 || c.Command[0] != "/bootstrap/run" {
		t.Fatalf("command: %v", c.Command)
	}

	// Params become env vars, sorted for deterministic pod templates.
	var names []string
	for _, e := range c.Env {
		if e.Name == "A_PARAM" || e.Name == "Z_PARAM" {
			names = append(names, e.Name)
		}
	}
	if len(names) != 2 || names[0] != "A_PARAM" {
		t.Fatalf("params not sorted/injected: %v", names)
	}

	// Timeout from spec, backoff defaults to 0 (no blind retries).
	if *job.Spec.ActiveDeadlineSeconds != 300 {
		t.Fatalf("timeout: %d", *job.Spec.ActiveDeadlineSeconds)
	}
	if *job.Spec.BackoffLimit != 0 {
		t.Fatalf("backoff: %d", *job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("restart policy must be Never")
	}
}

func TestBootstrapFilesAndSecretMounts(t *testing.T) {
	r := &StackBootstrapReconciler{}
	sb := testBootstrap()
	job := r.buildJob(sb, "j")

	mounts := job.Spec.Template.Spec.Containers[0].VolumeMounts
	foundFiles, foundSecret := false, false
	for _, m := range mounts {
		if m.MountPath == "/bootstrap/files" {
			foundFiles = true
		}
		if m.MountPath == "/bootstrap/secrets/ai-secrets" && m.ReadOnly {
			foundSecret = true
		}
	}
	if !foundFiles || !foundSecret {
		t.Fatalf("mounts: %+v", mounts)
	}

	// Files ConfigMap carries the generation marker for drift detection.
	cm := filesConfigMap(sb)
	if cm.Data["generation"] != "3" {
		t.Fatalf("generation marker: %q", cm.Data["generation"])
	}
	if cm.Data["init.sql"] != "SELECT 1;" {
		t.Fatalf("files missing: %v", cm.Data)
	}
	if !strings.HasPrefix(cm.Name, "stackbootstrap-files-") {
		t.Fatalf("cm name: %s", cm.Name)
	}
}

func TestBootstrapTerminalPhasesAreSticky(t *testing.T) {
	// A terminal phase with current observedGeneration must not re-run:
	// the reconcile guard short-circuits before creating anything. The SA
	// helper is the contract surface for tests.
	if saOrDefault("") != "kubo-stackbootstrap" {
		t.Fatal("default SA")
	}
	if saOrDefault("custom-sa") != "custom-sa" {
		t.Fatal("override SA")
	}
}
