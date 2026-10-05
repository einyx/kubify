package portal

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/einyx/kubo/api/v1alpha1"
)

func TestCreateDemoRequestEndpoint(t *testing.T) {
	t.Run("disabled without token secret", func(t *testing.T) {
		p := newFake(t)
		r := httptest.NewRequest(http.MethodPost, "http://localhost/api/demorequests", bytes.NewReader([]byte(`{"email":"a@b.io"}`)))
		w := httptest.NewRecorder()
		p.Mux().ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
	})
	t.Run("rejects bad token", func(t *testing.T) {
		p := newFake(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-requests-token", Namespace: "kubo-system"}, Data: map[string][]byte{"token": []byte("sekrit")}})
		r := httptest.NewRequest(http.MethodPost, "http://localhost/api/demorequests", bytes.NewReader([]byte(`{"email":"a@b.io"}`)))
		r.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()
		p.Mux().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	})
	t.Run("creates DemoRequest for valid submission", func(t *testing.T) {
		p := newFake(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-requests-token", Namespace: "kubo-system"}, Data: map[string][]byte{"token": []byte("sekrit")}})
		r := httptest.NewRequest(http.MethodPost, "http://localhost/api/demorequests", bytes.NewReader([]byte(`{"email":"Ada@Example.IO","company":"Acme Corp"}`)))
		r.Header.Set("Authorization", "Bearer sekrit")
		w := httptest.NewRecorder()
		p.Mux().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
		}
		var drs v1alpha1.DemoRequestList
		if err := p.client.List(context.Background(), &drs); err != nil {
			t.Fatal(err)
		}
		if len(drs.Items) != 1 {
			t.Fatalf("demorequests = %d, want 1", len(drs.Items))
		}
		dr := drs.Items[0]
		if dr.Spec.Email != "ada@example.io" || dr.Spec.Company != "Acme Corp" {
			t.Fatalf("spec = %+v", dr.Spec)
		}
		if dr.Spec.Template != "full" {
			t.Fatalf("template = %q, want full", dr.Spec.Template)
		}
	})
	t.Run("rejects invalid email", func(t *testing.T) {
		p := newFake(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "demo-requests-token", Namespace: "kubo-system"}, Data: map[string][]byte{"token": []byte("sekrit")}})
		r := httptest.NewRequest(http.MethodPost, "http://localhost/api/demorequests", bytes.NewReader([]byte(`{"email":"not-an-email"}`)))
		r.Header.Set("Authorization", "Bearer sekrit")
		w := httptest.NewRecorder()
		p.Mux().ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			t.Fatal("invalid email accepted")
		}
	})
}

func TestDemoRequestLifecycleEndpoints(t *testing.T) {
	p := newFake(t)
	// create one pending request directly
	now := metav1.Now()
	dr := &v1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-x", Namespace: "kubo-system", CreationTimestamp: now},
		Spec:       v1alpha1.DemoRequestSpec{Email: "x@y.io", Company: "X"},
		Status:     v1alpha1.DemoRequestStatus{ApprovedAt: now.UTC().Format(time.RFC3339)},
	}
	if err := p.client.Create(context.Background(), dr); err != nil {
		t.Fatal(err)
	}
	h := p.Mux()

	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil))
		return w
	}
	post := func(path string, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "http://localhost"+path, strings.NewReader(body)))
		return w
	}

	if w := get("/api/demorequests"); w.Code != 200 || !strings.Contains(w.Body.String(), "x@y.io") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if w := post("/api/demorequests/demo-x/approve", ""); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	var updated v1alpha1.DemoRequest
	if err := p.client.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "demo-x"}, &updated); err != nil {
		t.Fatal(err)
	}
	if !updated.Spec.Approved {
		t.Fatal("approve did not flip spec.approved")
	}
	if w := post("/api/demorequests/demo-x/extend", `{"hours":24}`); w.Code != 200 {
		t.Fatalf("extend: %d %s", w.Code, w.Body.String())
	}
	if err := p.client.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "demo-x"}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Spec.TTL == nil || updated.Spec.TTL.Duration < 23*time.Hour {
		t.Fatalf("extend did not set a ≥23h ttl: %+v", updated.Spec.TTL)
	}
	if w := post("/api/demorequests/demo-x/reject", ""); w.Code != 200 {
		t.Fatalf("reject: %d %s", w.Code, w.Body.String())
	}
}

func demoReq(name, email, company string, approved bool) *v1alpha1.DemoRequest {
	return &v1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kubo-system"},
		Spec:       v1alpha1.DemoRequestSpec{Email: email, Company: company, Approved: approved},
	}
}

func TestPatchDemoRequestEndpoint(t *testing.T) {
	p := newFake(t, demoReq("acme-corp", "typo@acme.io", "Acme", false))
	h := p.Mux()

	t.Run("updates email and company", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPatch, "http://localhost/api/demorequests/acme-corp",
			strings.NewReader(`{"email":"ops@acme.io","company":"Acme Corp Inc"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
		}
		var dr v1alpha1.DemoRequest
		if err := p.client.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "acme-corp"}, &dr); err != nil {
			t.Fatal(err)
		}
		if dr.Spec.Email != "ops@acme.io" || dr.Spec.Company != "Acme Corp Inc" {
			t.Fatalf("spec = %+v", dr.Spec)
		}
	})

	t.Run("rejects invalid email", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPatch, "http://localhost/api/demorequests/acme-corp",
			strings.NewReader(`{"email":"nope"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			t.Fatal("invalid email accepted")
		}
	})

	t.Run("empty patch rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPatch, "http://localhost/api/demorequests/acme-corp",
			strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusOK {
			t.Fatal("empty patch accepted")
		}
	})

	t.Run("404 for unknown name", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPatch, "http://localhost/api/demorequests/ghost",
			strings.NewReader(`{"email":"a@b.io"}`))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
}

func TestDeleteDemoRequestEndpoint(t *testing.T) {
	p := newFake(t, demoReq("acme-corp", "ops@acme.io", "Acme", true))
	h := p.Mux()

	r := httptest.NewRequest(http.MethodDelete, "http://localhost/api/demorequests/acme-corp", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var drs v1alpha1.DemoRequestList
	if err := p.client.List(context.Background(), &drs); err != nil {
		t.Fatal(err)
	}
	if len(drs.Items) != 0 {
		t.Fatalf("demorequests = %d, want 0", len(drs.Items))
	}
}
