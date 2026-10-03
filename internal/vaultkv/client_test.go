package vaultkv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeVault is an in-memory KV v2 server exercising the endpoints the client
// uses. It records the token of the last request for auth assertions.
type fakeVault struct {
	t        *testing.T
	mu       data            // path -> data
	metadata map[string]bool // paths with metadata (created via write)
	sealed   bool
	lastAuth map[string]string // auth/token/create requests
	token    string
	tokens   map[string]bool // accepted tokens (root + issued children)
	mux      *http.ServeMux
	srv      *httptest.Server
}

type data map[string]map[string]string

func newFakeVault(t *testing.T, sealed bool) *fakeVault {
	f := &fakeVault{t: t, mu: data{}, metadata: map[string]bool{}, sealed: sealed, token: "root-token", lastAuth: map[string]string{}, tokens: map[string]bool{"root-token": true}}
	f.mux = http.NewServeMux()
	f.mux.HandleFunc("/v1/sys/seal-status", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"sealed":` + boolStr(f.sealed) + `}`))
	})
	f.mux.HandleFunc("/v1/auth/token/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Policies []string `json:"policies"`
			TTL      string   `json:"ttl"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.lastAuth["policy"] = strings.Join(req.Policies, ",")
		f.lastAuth["ttl"] = req.TTL
		f.tokens["child-token"] = true
		w.Write([]byte(`{"auth":{"client_token":"child-token"}}`))
	})
	f.mux.HandleFunc("/v1/secret/data/", f.handleData)
	f.mux.HandleFunc("/v1/secret/metadata/", f.handleMetadata)
	f.srv = httptest.NewServer(f.mux)
	t.Cleanup(f.srv.Close)
	return f
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (f *fakeVault) authed(w http.ResponseWriter, r *http.Request) bool {
	if !f.tokens[r.Header.Get("X-Vault-Token")] {
		w.WriteHeader(http.StatusForbidden)
		return false
	}
	return true
}

func (f *fakeVault) handleData(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
	switch r.Method {
	case http.MethodGet:
		d, ok := f.mu[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"data":{"data":` + mustJSON(d) + `,"metadata":{"version":2,"created_time":"2026-01-01T00:00:00Z","destroyed":false}}}`))
	case http.MethodPost:
		var req struct {
			Data map[string]string `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu[path] = req.Data
		f.metadata[path] = true
		w.Write([]byte(`{"data":{"version":2}}`))
	case http.MethodDelete:
		delete(f.mu, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeVault) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/")
	switch r.Method {
	case http.MethodGet:
		if strings.HasSuffix(r.URL.RawQuery, "list=true") || r.URL.Query().Get("list") == "true" {
			var keys []string
			for p := range f.metadata {
				if rest, ok := trimPrefixEntry(p, path); ok {
					keys = append(keys, rest)
				}
			}
			if len(keys) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Write([]byte(`{"data":{"keys":` + mustJSON(keys) + `}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case http.MethodDelete:
		delete(f.metadata, path)
		delete(f.mu, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func trimPrefixEntry(p, prefix string) (string, bool) {
	if prefix != "" {
		if !strings.HasPrefix(p, prefix+"/") {
			return "", false
		}
		p = p[len(prefix)+1:]
	}
	if p == "" {
		return "", false
	}
	return p, true
}

func mustJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func clientFor(t *testing.T, addr, token string) *Client {
	c := &Client{addr: addr, token: token, http: &http.Client{}}
	return c
}

func secretScheme(t *testing.T) *runtime.Scheme {
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func TestNewForNamespaceReadsRootToken(t *testing.T) {
	unseal := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-unseal-keys", Namespace: "tenant-a"},
		Data:       map[string][]byte{"vault-root": []byte("root-token")},
	}
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(unseal).Build()

	vc, err := NewForNamespace(context.Background(), c, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	if vc.Addr() != "http://vault.tenant-a.svc.cluster.local:8200" {
		t.Errorf("addr = %q", vc.Addr())
	}
	if vc.token != "root-token" {
		t.Errorf("token not read from secret")
	}
}

func TestNewForNamespaceErrors(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(secretScheme(t)).Build()
	if _, err := NewForNamespace(context.Background(), c, "nope"); err == nil || !apierrors.IsNotFound(err) && !strings.Contains(err.Error(), "no Vault") {
		t.Errorf("missing namespace: want friendly error, got %v", err)
	}

	partial := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-unseal-keys", Namespace: "ns2"},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	c2 := fake.NewClientBuilder().WithScheme(secretScheme(t)).WithObjects(partial).Build()
	if _, err := NewForNamespace(context.Background(), c2, "ns2"); err == nil || !strings.Contains(err.Error(), "vault-root") {
		t.Errorf("missing root key: got %v", err)
	}
}

func TestReadWriteAndVersionMetadata(t *testing.T) {
	f := newFakeVault(t, false)
	c := clientFor(t, f.srv.URL, "root-token")

	if err := c.Write(context.Background(), "app/config", map[string]string{"A": "1", "B": "2"}); err != nil {
		t.Fatal(err)
	}
	e, err := c.Read(context.Background(), "app/config")
	if err != nil {
		t.Fatal(err)
	}
	if e.Data["A"] != "1" || e.Version != 2 || e.CreatedAt == "" {
		t.Errorf("entry = %+v", e)
	}
}

func TestReadMissing(t *testing.T) {
	f := newFakeVault(t, false)
	c := clientFor(t, f.srv.URL, "root-token")
	if _, err := c.Read(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("want 404 error, got %v", err)
	}
}

func TestListAndSplit(t *testing.T) {
	f := newFakeVault(t, false)
	c := clientFor(t, f.srv.URL, "root-token")
	f.metadata["app/config"] = true
	f.metadata["app/sub/"] = true

	keys, err := c.List(context.Background(), "app")
	if err != nil {
		t.Fatal(err)
	}
	entries, folders := SplitList(keys)
	if len(entries) != 1 || entries[0] != "config" {
		t.Errorf("entries = %v", entries)
	}
	if len(folders) != 1 || folders[0] != "sub/" {
		t.Errorf("folders = %v", folders)
	}
}

func TestDeleteSoftVsPermanent(t *testing.T) {
	f := newFakeVault(t, false)
	c := clientFor(t, f.srv.URL, "root-token")
	f.mu["app/x"] = map[string]string{"k": "v"}
	f.metadata["app/x"] = true

	if err := c.Delete(context.Background(), "app/x", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.mu["app/x"]; ok {
		t.Error("soft delete should remove the data")
	}
	if !f.metadata["app/x"] {
		t.Error("soft delete should keep metadata (versions recoverable)")
	}

	if err := c.Delete(context.Background(), "app/x", true); err != nil {
		t.Fatal(err)
	}
	if f.metadata["app/x"] {
		t.Error("permanent delete should remove metadata")
	}
}

func TestSealed(t *testing.T) {
	c := clientFor(t, newFakeVault(t, true).srv.URL, "root-token")
	sealed, err := c.Sealed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sealed {
		t.Error("expected sealed")
	}
}

func TestWrongTokenRejected(t *testing.T) {
	f := newFakeVault(t, false)
	c := clientFor(t, f.srv.URL, "wrong-token")
	if _, err := c.Read(context.Background(), "app/x"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("want 403, got %v", err)
	}
}

func TestNewWithChildToken(t *testing.T) {
	f := newFakeVault(t, false)
	root := clientFor(t, f.srv.URL, "root-token")

	child, err := root.NewWithChildToken(context.Background(), "kubo-portal", "5m")
	if err != nil {
		t.Fatal(err)
	}
	if child.token != "child-token" {
		t.Errorf("child token = %q", child.token)
	}
	if f.lastAuth["policy"] != "kubo-portal" || f.lastAuth["ttl"] != "5m" {
		t.Errorf("create params = %v", f.lastAuth)
	}
	// Child token works for reads.
	f.mu["app/x"] = map[string]string{"k": "v"}
	f.metadata["app/x"] = true
	if _, err := child.Read(context.Background(), "app/x"); err != nil {
		t.Errorf("child token read: %v", err)
	}
}

var _ = types.NamespacedName{} // keep import if unused after refactors
