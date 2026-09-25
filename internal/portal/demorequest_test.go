package portal

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
