package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFake(t *testing.T, objs ...client.Object) *Portal {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return New(fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build())
}

func TestRenderTemplateSubstitutesTenant(t *testing.T) {
	objs, err := renderTemplate("demo-b")
	if err != nil {
		t.Fatal(err)
	}
	ns, ok := objs[0].(*corev1.Namespace)
	if !ok {
		t.Fatalf("obj[0] is %T, want Namespace", objs[0])
	}
	if ns.Name != "product-demo-b" {
		t.Fatalf("namespace: %q", ns.Name)
	}
	stack, ok := objs[1].(*v1alpha1.Stack)
	if !ok {
		t.Fatalf("obj[1] is %T, want Stack", objs[1])
	}
	if stack.Namespace != "product-demo-b" {
		t.Fatalf("stack namespace: %q", stack.Namespace)
	}
	// Secrets propagate from kubo-system with the tenant prefix.
	found := false
	for _, ref := range stack.Spec.SecretsRef {
		if ref.From == "product-demo-b-backend-auth0" {
			found = true
		}
	}
	if !found {
		t.Fatal("secretRef prefix not templated for tenant")
	}
	// No raw template placeholder may survive.
	b, _ := json.Marshal(stack)
	if strings.Contains(string(b), "{{.Tenant}}") {
		t.Fatal("unsubstituted placeholder left in rendered stack")
	}
}

func TestCreateFromTemplateValidation(t *testing.T) {
	p := newFake(t)
	if _, err := p.CreateFromTemplate(context.Background(), "Bad_Tenant", "Direct", false); err == nil {
		t.Fatal("invalid tenant accepted")
	}
	out, err := p.CreateFromTemplate(context.Background(), "demo-b", "Direct", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: product-demo-b") {
		t.Fatal("dry run did not return rendered YAML")
	}
	// Dry run must not persist anything.
	stacks, err := p.ListStacks(context.Background())
	if err != nil || len(stacks) != 0 {
		t.Fatalf("dry run created resources: %v %v", stacks, err)
	}
}

func TestCreateFromTemplatePersistsAndModes(t *testing.T) {
	p := newFake(t)
	if _, err := p.CreateFromTemplate(context.Background(), "demo-b", "Flux", false); err != nil {
		t.Fatal(err)
	}
	stacks, err := p.ListStacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks) != 1 || stacks[0].Namespace != "product-demo-b" || stacks[0].Mode != "Flux" {
		t.Fatalf("unexpected stacks: %+v", stacks)
	}
	// Idempotent re-create (AlreadyExists tolerated).
	if _, err := p.CreateFromTemplate(context.Background(), "demo-b", "Direct", false); err != nil {
		t.Fatalf("re-create should tolerate AlreadyExists: %v", err)
	}
}

func TestListAndGetStacks(t *testing.T) {
	stack := &v1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{
			Name: "product", Namespace: "product-a",
			CreationTimestamp: metav1.Now(),
		},
		Status: v1alpha1.StackStatus{
			Phase: "Progressing",
			Components: []v1alpha1.ComponentStatus{
				{Name: "postgres", Phase: v1alpha1.ComponentPhaseReady, Revision: 7},
				{Name: "backend", Phase: v1alpha1.ComponentPhaseFailed, Message: "boom"},
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

	d, err := p.GetStack(context.Background(), "product-a", "product")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Components) != 2 || d.Components[0].Revision != 7 {
		t.Fatalf("unexpected detail: %+v", d)
	}
	if _, err := p.GetStack(context.Background(), "product-a", "nope"); err == nil {
		t.Fatal("missing stack should 404")
	}
}

func TestHTTPMux(t *testing.T) {
	p := newFake(t)
	h := p.Mux()

	// UI is served.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "kubo") {
		t.Fatalf("index: %d", rec.Code)
	}

	// Template preview is YAML with the tenant substituted.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/template?tenant=web", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "product-web") {
		t.Fatalf("template preview: %d %s", rec.Code, rec.Body.String())
	}

	// Create via POST, then list + detail round-trip.
	body := strings.NewReader(`{"tenant":"web","mode":"Direct"}`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/stacks", body))
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stacks", nil))
	var stacks []StackSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &stacks); err != nil || len(stacks) != 1 {
		t.Fatalf("list after create: %s (%v)", rec.Body.String(), err)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stacks/product-web/product", nil))
	if rec.Code != 200 {
		t.Fatalf("detail: %d", rec.Code)
	}

	// Invalid tenant is a 400 with a JSON error the UI can show.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/stacks",
		strings.NewReader(`{"tenant":"NOT VALID"}`)))
	if rec.Code != 400 {
		t.Fatalf("invalid tenant: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("error body: %s", rec.Body.String())
	}
}
