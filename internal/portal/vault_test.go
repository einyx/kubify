package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/einyx/kubo/internal/vaultkv/vaultkvtest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// vaultFixture mounts the in-memory Vault behind the portal's addr override,
// plus the vault-unseal-keys Secret the plumbing reads.
func vaultFixture(t *testing.T, p *Portal) *vaultkvtest.Fake {
	f := vaultkvtest.New(t)
	p.SetVaultAddrFunc(func(ns string) string { return f.Addr() })
	unseal := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-unseal-keys", Namespace: "acme-demo"},
		Data:       map[string][]byte{"vault-root": []byte("root-token")},
	}
	if err := p.client.Create(context.Background(), unseal); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVaultHealthAndGuards(t *testing.T) {
	p := newFake(t, testStack("acme-demo"))
	f := vaultFixture(t, p)
	h := p.Mux()

	// health: installed + unsealed
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/namespaces/acme-demo/vault/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}
	var health VaultHealth
	_ = json.Unmarshal(rec.Body.Bytes(), &health)
	if !health.Installed || health.Sealed {
		t.Errorf("health = %+v", health)
	}

	// invalid namespace rejected
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/namespaces/Bad_NS/vault/health", nil))
	if rec.Code != 400 {
		t.Errorf("invalid ns: %d", rec.Code)
	}

	// sealed vault surfaces as an error response
	f.SetSealed(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/namespaces/acme-demo/vault/tree", nil))
	if rec.Code != 400 {
		t.Errorf("sealed tree: %d %s", rec.Code, rec.Body.String())
	}
	f.SetSealed(false)
}

func TestVaultEntryRedactionAndReveal(t *testing.T) {
	p := newFake(t, testStack("acme-demo"))
	vaultFixture(t, p)
	h := p.Mux()
	ctx := context.Background()

	// Seed an entry through the client directly.
	vc, err := p.vaultClientFor(ctx, "acme-demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := vc.Write(ctx, "app/config", map[string]string{"SECRET": "hunter2", "PLAIN": "ok"}); err != nil {
		t.Fatal(err)
	}

	// Without reveal: values redacted, keys present.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/namespaces/acme-demo/vault/entry?path=app/config", nil))
	if rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
	}
	var view VaultEntryView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Data["SECRET"] != "••••••••" || !strings.Contains(rec.Body.String(), "hunter2") == false {
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatal("secret value leaked without reveal=true")
		}
	}
	if len(view.Keys) != 2 {
		t.Errorf("keys = %v", view.Keys)
	}

	// With reveal: real values come back.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/namespaces/acme-demo/vault/entry?path=app/config&reveal=true", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Data["SECRET"] != "hunter2" {
		t.Errorf("reveal did not return the value: %v", view.Data)
	}
}

func TestVaultWriteListDelete(t *testing.T) {
	p := newFake(t, testStack("acme-demo"))
	f := vaultFixture(t, p)
	h := p.Mux()

	// Write.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "http://localhost/api/namespaces/acme-demo/vault/entry",
		strings.NewReader(`{"path":"app/config","data":{"TOKEN":"abc"}}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("write: %d %s", rec.Code, rec.Body.String())
	}

	// Tree shows the entry.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/namespaces/acme-demo/vault/tree?path=app", nil))
	if !strings.Contains(rec.Body.String(), "config") {
		t.Errorf("tree missing entry: %s", rec.Body.String())
	}

	// Path traversal rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("PUT", "http://localhost/api/namespaces/acme-demo/vault/entry",
		strings.NewReader(`{"path":"../evil","data":{"A":"B"}}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("traversal: %d", rec.Code)
	}

	// Soft delete.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "http://localhost/api/namespaces/acme-demo/vault/entry?path=app/config", nil))
	if rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}

	// Permanent delete requires confirm=<namespace>.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "http://localhost/api/namespaces/acme-demo/vault/entry?path=app/config&permanent=true", nil))
	if rec.Code != 400 {
		t.Errorf("permanent without confirm: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "http://localhost/api/namespaces/acme-demo/vault/entry?path=app/config&permanent=true&confirm=acme-demo", nil))
	if rec.Code != 200 {
		t.Errorf("permanent with confirm: %d %s", rec.Code, rec.Body.String())
	}
	if f.Exists("app/config") {
		t.Error("permanent delete should destroy metadata")
	}

}
