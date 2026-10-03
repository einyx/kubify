package webhook

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func clientKey(ns, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: ns, Name: name}
}

func handler(t *testing.T) (*ProvisioningHandler, *strings.Builder) {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	return &ProvisioningHandler{Client: c, SecretKey: "s3cret"}, &strings.Builder{}
}

func post(t *testing.T, h *ProvisioningHandler, secret string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/provision", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProvisionUnauthorized(t *testing.T) {
	h, _ := handler(t)
	rec := post(t, h, "wrong-secret", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	// missing header entirely
	rec = post(t, h, "", `{}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestProvisionBadJSON(t *testing.T) {
	h, _ := handler(t)
	rec := post(t, h, "s3cret", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rec.Code)
	}
}

func TestProvisionCreatesNamespaceAndStack(t *testing.T) {
	h, _ := handler(t)
	rec := post(t, h, "s3cret", `{
		"customer_name": "acme",
		"namespace": "acme-ns",
		"stack_ref": "product",
		"values": {"replicas": 3, "tier": "standard"}
	}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v", err)
	}
	if resp["status"] != "provisioning" || resp["name"] != "acme" {
		t.Errorf("response wrong: %v", resp)
	}

	var stack platformv1alpha1.Stack
	if err := h.Client.Get(t.Context(), clientKey("acme-ns", "acme"), &stack); err != nil {
		t.Fatalf("Stack not created: %v", err)
	}
	if stack.Spec.StackRef != "product" {
		t.Errorf("stackRef = %q", stack.Spec.StackRef)
	}
	if !strings.Contains(string(stack.Spec.Values.Raw), `"replicas":3`) {
		t.Errorf("values not embedded: %s", stack.Spec.Values.Raw)
	}
	// Namespace was created.
	var ns corev1.Namespace
	if err := h.Client.Get(t.Context(), clientKey("", "acme-ns"), &ns); err != nil {
		t.Errorf("Namespace not created: %v", err)
	}
}

func TestProvisionIdempotentNamespace(t *testing.T) {
	h, _ := handler(t)
	body := `{"customer_name":"acme","namespace":"acme-ns","stack_ref":"product"}`
	if rec := post(t, h, "s3cret", body); rec.Code != http.StatusCreated {
		t.Fatalf("first provision: %d", rec.Code)
	}
	// Second request: namespace already exists (OK), but the Stack create
	// collides -> internal error is acceptable, or 201 if names differ.
	rec := post(t, h, "s3cret", `{"customer_name":"acme","namespace":"acme-ns","stack_ref":"product"}`)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusInternalServerError {
		t.Errorf("second provision: %d", rec.Code)
	}
}
