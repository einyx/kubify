package portal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGetStackYAMLAndMissing(t *testing.T) {
	ctx := context.Background()
	p := newFake(t, testStack("acme-demo"))

	y, err := p.GetStackYAML(ctx, "acme-demo", "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: Stack", "apiVersion: platform.kubo.io/v1alpha1", "name: acme"} {
		if !strings.Contains(y, want) {
			t.Errorf("yaml missing %q:\n%s", want, y)
		}
	}
	if _, err := p.GetStackYAML(ctx, "acme-demo", "nope"); err == nil {
		t.Error("expected error for missing stack")
	}
}

func TestCreateStackBackupS3Only(t *testing.T) {
	ctx := context.Background()
	p := newFake(t, testStack("acme-demo"))

	// s3-only backups do not require postgres.database/user.
	bk, err := p.CreateStackBackup(ctx, BackupRequest{
		SourceNamespace: "acme-demo",
		TargetNamespace: "stack-b",
		Include:         []string{"s3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bk.Phase != "Pending" {
		t.Errorf("phase = %q", bk.Phase)
	}
}

func TestCreateFromTemplateUnknownTemplate(t *testing.T) {
	ctx := context.Background()
	p := newFake(t)
	if _, err := p.CreateFromTemplate(ctx, CreateRequest{Template: "nope", Tenant: "web"}, false); err == nil {
		t.Error("expected error for unknown template")
	}
}

func TestPatchStackSpecComponentValuesRemoval(t *testing.T) {
	ctx := context.Background()
	p := newFake(t, &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme-demo"},
		Spec: platformv1alpha1.StackSpec{
			ComponentValues: map[string]apiextensionsv1.JSON{
				"backend": {Raw: []byte(`{"replicas":2}`)},
				"worker":  {Raw: []byte(`{"concurrency":8}`)},
			},
		},
	})

	// nil value removes the override; non-nil replaces it.
	one := json.RawMessage(`{"replicas":5}`)
	if err := p.PatchStackSpec(ctx, "acme-demo", "acme", PatchRequest{
		ComponentValues: map[string]json.RawMessage{"backend": nil, "worker": one},
	}); err != nil {
		t.Fatal(err)
	}
	var s platformv1alpha1.Stack
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: "acme-demo", Name: "acme"}, &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Spec.ComponentValues["backend"]; ok {
		t.Error("backend override should be removed")
	}
	if string(s.Spec.ComponentValues["worker"].Raw) != `{"replicas":5}` {
		t.Errorf("worker override = %s", s.Spec.ComponentValues["worker"].Raw)
	}

	// Removing the last override nils the map.
	if err := p.PatchStackSpec(ctx, "acme-demo", "acme", PatchRequest{
		ComponentValues: map[string]json.RawMessage{"worker": nil},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: "acme-demo", Name: "acme"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Spec.ComponentValues != nil {
		t.Errorf("componentValues should be nil, got %v", s.Spec.ComponentValues)
	}
}

func TestPatchStackSpecInvalidComponentValuesJSON(t *testing.T) {
	ctx := context.Background()
	p := newFake(t, testStack("acme-demo"))
	err := p.PatchStackSpec(ctx, "acme-demo", "acme", PatchRequest{
		ComponentValues: map[string]json.RawMessage{
			"backend": json.RawMessage(`{not-json`),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("want invalid-JSON error, got %v", err)
	}
}
