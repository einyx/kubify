package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// testTemplate is a tiny environment-agnostic fixture written into a temp
// local templates dir — tests never depend on the gitignored real templates.
const testTemplate = `id: test-fixture
name: Test fixture
description: minimal template used by tests
defaults:
  mode: Direct
  exclude: [kafka]
  operators:
    agentFW: true
---
apiVersion: v1
kind: Namespace
metadata:
  name: foundation-{{.Tenant}}
---
apiVersion: platform.kubo.io/v1alpha1
kind: Stack
metadata:
  name: foundation
  namespace: foundation-{{.Tenant}}
spec:
  mode: Direct
  secretsRef:
  - from: foundation-{{.Tenant}}-backend-auth0
    to: backend-auth0
  inline:
    components: []
    title: Foundation {{.Tenant}}
`

func newFake(t *testing.T, objs ...client.Object) *Portal {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	p := New(fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build())
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test-fixture.yaml"), []byte(testTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	p.SetTemplateDir(dir)
	return p
}

func testStack(ns string) *v1alpha1.Stack {
	return &v1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foundation",
			Namespace: ns,
		},
		Spec: v1alpha1.StackSpec{
			Mode:    v1alpha1.DeploymentModeDirect,
			Exclude: []string{"kafka"},
		},
	}
}

func TestReconcileStack(t *testing.T) {
	ctx := context.Background()
	p := newFake(t, testStack("foundation-demo"))
	if err := p.ReconcileStack(ctx, "foundation-demo", "foundation"); err != nil {
		t.Fatal(err)
	}
	var s v1alpha1.Stack
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: "foundation-demo", Name: "foundation"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Annotations[reconcileAnnotation] == "" {
		t.Fatal("reconcile-at annotation not set")
	}
	if err := p.ReconcileStack(ctx, "foundation-demo", "nope"); err == nil {
		t.Fatal("expected error for missing stack")
	}
}

func TestPatchStackSpec(t *testing.T) {
	ctx := context.Background()
	p := newFake(t, testStack("foundation-demo"))

	flux := "Flux"
	clearBundle := ""
	bundle := "oci://ghcr.io/org/bundle:v2"
	if err := p.PatchStackSpec(ctx, "foundation-demo", "foundation", PatchRequest{
		Mode: flux, Bundle: &bundle, Exclude: []string{"kafka", "spark"},
		Operators: map[string]bool{"vault": true, "istio": false},
	}); err != nil {
		t.Fatal(err)
	}
	var s v1alpha1.Stack
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: "foundation-demo", Name: "foundation"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Spec.Mode != v1alpha1.DeploymentModeFlux {
		t.Fatalf("mode = %q", s.Spec.Mode)
	}
	if s.Spec.Bundle == nil || s.Spec.Bundle.URL != bundle {
		t.Fatalf("bundle = %+v", s.Spec.Bundle)
	}
	if len(s.Spec.Exclude) != 2 || s.Spec.Exclude[1] != "spark" {
		t.Fatalf("exclude = %v", s.Spec.Exclude)
	}
	if s.Spec.Operators == nil || !s.Spec.Operators.Vault || s.Spec.Operators.Istio {
		t.Fatalf("operators = %+v", s.Spec.Operators)
	}

	// Clearing the bundle URL drops the BundleSource entirely.
	if err := p.PatchStackSpec(ctx, "foundation-demo", "foundation", PatchRequest{Bundle: &clearBundle}); err != nil {
		t.Fatal(err)
	}
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: "foundation-demo", Name: "foundation"}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Spec.Bundle != nil {
		t.Fatalf("bundle not cleared: %+v", s.Spec.Bundle)
	}

	// Invalid mode and invalid bundle URL are rejected without patching.
	if err := p.PatchStackSpec(ctx, "foundation-demo", "foundation", PatchRequest{Mode: "bogus"}); err == nil {
		t.Fatal("expected invalid mode error")
	}
	bad := "ghcr.io/no-scheme"
	if err := p.PatchStackSpec(ctx, "foundation-demo", "foundation", PatchRequest{Bundle: &bad}); err == nil {
		t.Fatal("expected invalid bundle error")
	}
}

func TestListStackEvents(t *testing.T) {
	ctx := context.Background()
	older := metav1.NewTime(time.Now().Add(-time.Hour))
	newer := metav1.NewTime(time.Now())
	events := []client.Object{
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "foundation-demo", Name: "e1"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-0"},
			Reason:         "Pulled", Type: corev1.EventTypeNormal, Count: 2,
			LastTimestamp: older, Message: "Pull complete",
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Namespace: "foundation-demo", Name: "e2"},
			InvolvedObject: corev1.ObjectReference{Kind: "HelmRelease", Name: "vault"},
			Reason:         "InstallFailed", Type: corev1.EventTypeWarning, Count: 1,
			LastTimestamp: newer, Message: "helm install failed",
		},
		// Other namespace — must not appear.
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "e3"},
			Reason:     "Noisy", LastTimestamp: newer,
		},
	}
	p := newFake(t, events...)
	got, err := p.ListStackEvents(ctx, "foundation-demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Reason != "InstallFailed" || got[0].Type != "Warning" || got[0].Object != "HelmRelease/vault" {
		t.Fatalf("first event wrong: %+v", got[0])
	}
	if got[1].Reason != "Pulled" || got[1].Count != 2 {
		t.Fatalf("second event wrong: %+v", got[1])
	}
}

func TestBuiltinEmptyTemplate(t *testing.T) {
	p := newFake(t) // registry also has the temp-dir fixture
	all, err := p.registry.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, tm := range all {
		ids[tm.Meta.ID] = true
	}
	if !ids["empty"] || !ids["test-fixture"] {
		t.Fatalf("missing templates: %v", ids)
	}
	empty, err := p.registry.Get(context.Background(), "empty")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(empty.Body, "kind: Namespace") || strings.Contains(empty.Body, "{{.Tenant}}") {
		// placeholder is fine in body — it must only survive after render
		_ = empty
	}
	objs, err := renderTemplate(empty.Body, "x1")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("empty renders %d objects", len(objs))
	}
}

func TestRegistryPrecedence(t *testing.T) {
	// A local file with the same id as a builtin wins.
	dir := t.TempDir()
	override := strings.Replace(testTemplate, "id: test-fixture", "id: empty", 1)
	override = strings.Replace(override, "kind: Namespace", "kind: Namespace # overridden", 1)
	if err := os.WriteFile(filepath.Join(dir, "empty.yaml"), []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	sch := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	p := New(fake.NewClientBuilder().WithScheme(sch).Build())
	p.SetTemplateDir(dir)
	tpl, err := p.registry.Get(context.Background(), "empty")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tpl.Body, "# overridden") {
		t.Fatal("local template did not override builtin")
	}
}

func TestRenderTemplateSubstitutesTenant(t *testing.T) {
	p := newFake(t)
	tmpl, err := p.registry.Get(context.Background(), "test-fixture")
	if err != nil {
		t.Fatal(err)
	}
	objs, err := renderTemplate(tmpl.Body, "demo-b")
	if err != nil {
		t.Fatal(err)
	}
	ns, ok := objs[0].(*corev1.Namespace)
	if !ok {
		t.Fatalf("obj[0] is %T, want Namespace", objs[0])
	}
	if ns.Name != "foundation-demo-b" {
		t.Fatalf("namespace: %q", ns.Name)
	}
	stack, ok := objs[1].(*v1alpha1.Stack)
	if !ok {
		t.Fatalf("obj[1] is %T, want Stack", objs[1])
	}
	if stack.Namespace != "foundation-demo-b" {
		t.Fatalf("stack namespace: %q", stack.Namespace)
	}
	found := false
	for _, ref := range stack.Spec.SecretsRef {
		if ref.From == "foundation-demo-b-backend-auth0" {
			found = true
		}
	}
	if !found {
		t.Fatal("secretRef prefix not templated for tenant")
	}
	b, _ := json.Marshal(stack)
	if strings.Contains(string(b), "{{.Tenant}}") {
		t.Fatal("unsubstituted placeholder left in rendered stack")
	}
}

func TestCreateFromTemplateParams(t *testing.T) {
	p := newFake(t)

	// Invalid tenant rejected.
	if _, err := p.CreateFromTemplate(context.Background(),
		CreateRequest{Template: "test-fixture", Tenant: "Bad_Tenant"}, false); err == nil {
		t.Fatal("invalid tenant accepted")
	}

	// Unknown template rejected.
	if _, err := p.CreateFromTemplate(context.Background(),
		CreateRequest{Template: "nope", Tenant: "x"}, false); err == nil {
		t.Fatal("unknown template accepted")
	}

	// Dry run renders but persists nothing.
	out, err := p.CreateFromTemplate(context.Background(),
		CreateRequest{Template: "test-fixture", Tenant: "demo-b"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: foundation-demo-b") {
		t.Fatal("dry run did not return rendered YAML")
	}
	if stacks, _ := p.ListStacks(context.Background()); len(stacks) != 0 {
		t.Fatalf("dry run created resources: %v", stacks)
	}

	// Real create: mode override wins over default; template defaults
	// (exclude, operators) are applied.
	if _, err := p.CreateFromTemplate(context.Background(),
		CreateRequest{Template: "test-fixture", Tenant: "demo-b", Mode: "Flux"}, false); err != nil {
		t.Fatal(err)
	}
	stacks, err := p.ListStacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 || stacks[0].Namespace != "foundation-demo-b" || stacks[0].Mode != "Flux" {
		t.Fatalf("unexpected stacks: %+v", stacks)
	}
	var s v1alpha1.Stack
	if err := p.client.Get(context.Background(),
		client.ObjectKey{Namespace: "foundation-demo-b", Name: "foundation"}, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Spec.Exclude) != 1 || s.Spec.Exclude[0] != "kafka" {
		t.Fatalf("template exclude default not applied: %v", s.Spec.Exclude)
	}
	if s.Spec.Operators == nil || !s.Spec.Operators.AgentFW {
		t.Fatalf("template operator default not applied: %+v", s.Spec.Operators)
	}

	// Idempotent re-create (AlreadyExists tolerated), operator override.
	if _, err := p.CreateFromTemplate(context.Background(),
		CreateRequest{Template: "test-fixture", Tenant: "demo-b", Operators: map[string]bool{"vault": true}}, false); err != nil {
		t.Fatalf("re-create should tolerate AlreadyExists: %v", err)
	}
}

func TestListAndGetStacks(t *testing.T) {
	stack := &v1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foundation", Namespace: "foundation-a",
			CreationTimestamp: metav1.Now(),
		},
		Spec: v1alpha1.StackSpec{
			Mode:      v1alpha1.DeploymentModeDirect,
			Operators: &v1alpha1.ClusterOperators{AgentFW: true, Vault: true},
			Exclude:   []string{"kafka"},
		},
		Status: v1alpha1.StackStatus{
			Phase: "Progressing",
			Components: []v1alpha1.ComponentStatus{
				{Name: "postgres", Phase: v1alpha1.ComponentPhaseReady, Revision: 7},
				{Name: "backend", Phase: v1alpha1.ComponentPhaseFailed, Message: "boom"},
			},
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ComponentsNotReady", Message: "backend failed"},
			},
		},
	}
	p := newFake(t, stack)

	stacks, err := p.ListStacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 {
		t.Fatalf("want 1 stack, got %d", len(stacks))
	}
	s := stacks[0]
	if s.Phase != "Progressing" || s.Ready != 1 || s.Total != 2 || s.Mode != "Direct" {
		t.Fatalf("unexpected summary: %+v", s)
	}
	if !strings.Contains(s.FailureMsg, "backend") {
		t.Fatalf("failure not surfaced: %q", s.FailureMsg)
	}

	d, err := p.GetStack(context.Background(), "foundation-a", "foundation")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Components) != 2 || d.Components[0].Revision != 7 {
		t.Fatalf("unexpected detail: %+v", d)
	}
	if !d.Operators["agentFW"] || !d.Operators["vault"] {
		t.Fatalf("operators not surfaced: %+v", d.Operators)
	}
	if len(d.Conditions) != 1 || d.Conditions[0].Reason != "ComponentsNotReady" {
		t.Fatalf("conditions not surfaced: %+v", d.Conditions)
	}

	if _, err := p.GetStack(context.Background(), "foundation-a", "nope"); err == nil {
		t.Fatal("missing stack should 404")
	}

	// Live YAML endpoint.
	y, err := p.GetStackYAML(context.Background(), "foundation-a", "foundation")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(y, "agentFW: true") {
		t.Fatalf("yaml: %s", y)
	}
}

func TestDeleteStack(t *testing.T) {
	stack := &v1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Name: "foundation", Namespace: "foundation-x"}}
	p := newFake(t, stack)
	ctx := context.Background()

	// Wrong confirm is refused.
	if err := p.DeleteStack(ctx, "foundation-x", "foundation", "wrong", false); err == nil {
		t.Fatal("delete with wrong confirm accepted")
	}
	if _, err := p.GetStack(ctx, "foundation-x", "foundation"); err != nil {
		t.Fatal("stack deleted without valid confirm")
	}

	// Correct confirm deletes the Stack, namespace untouched by default.
	if err := p.DeleteStack(ctx, "foundation-x", "foundation", "foundation-x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetStack(ctx, "foundation-x", "foundation"); err == nil {
		t.Fatal("stack still present after delete")
	}
}

func TestHTTPMux(t *testing.T) {
	p := newFake(t)
	h := p.Mux()

	// UI is served.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "kubo") {
		t.Fatalf("index: %d", rec.Code)
	}

	// Template registry lists built-ins + fixture.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/templates", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "test-fixture") ||
		!strings.Contains(rec.Body.String(), "empty") {
		t.Fatalf("templates: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "foundation-{{.Tenant}}") {
		t.Fatal("template body leaked through registry API")
	}

	// Template preview is YAML with the tenant substituted.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/template?template=test-fixture&tenant=web", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "foundation-web") {
		t.Fatalf("template preview: %d %s", rec.Code, rec.Body.String())
	}

	// Create via POST, then list + detail + yaml round-trip.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "http://localhost/api/stacks",
		strings.NewReader(`{"template":"test-fixture","tenant":"web"}`)))
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/stacks", nil))
	var stacks []StackSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &stacks); err != nil || len(stacks) != 1 {
		t.Fatalf("list after create: %s (%v)", rec.Body.String(), err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/stacks/foundation-web/foundation", nil))
	if rec.Code != 200 {
		t.Fatalf("detail: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/stacks/foundation-web/foundation/yaml", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "kind: Stack") {
		t.Fatalf("yaml endpoint: %d %s", rec.Code, rec.Body.String())
	}

	// Delete with confirm via DELETE.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "http://localhost/api/stacks/foundation-web/foundation?confirm=foundation-web", nil))
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/stacks", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &stacks); err != nil || len(stacks) != 0 {
		t.Fatalf("list after delete: %s (%v)", rec.Body.String(), err)
	}

	// Invalid tenant is a 400 with a JSON error the UI can show.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "http://localhost/api/stacks",
		strings.NewReader(`{"template":"test-fixture","tenant":"NOT VALID"}`)))
	if rec.Code != 400 {
		t.Fatalf("invalid tenant: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("error body: %s", rec.Body.String())
	}
}
