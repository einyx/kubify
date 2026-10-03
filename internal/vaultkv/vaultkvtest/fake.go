// Package vaultkvtest provides an in-memory Vault KV v2 server for tests in
// other packages. It lives outside vaultkv so it can depend on net/http/httptest
// without bloating the production module.
package vaultkvtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Fake is an in-memory Vault implementing the endpoints the vaultkv client
// uses: sys/seal-status, auth/token/create, KV v2 data + metadata.
type Fake struct {
	mu       sync.Mutex
	store    map[string]map[string]string
	metadata map[string]bool
	sealed   bool
	tokens   map[string]bool
	tokenSeq int
	srv      *httptest.Server
}

// New starts the fake and returns it; the server is closed on test cleanup.
func New(t Tester) *Fake {
	f := &Fake{
		store:    map[string]map[string]string{},
		metadata: map[string]bool{},
		tokens:   map[string]bool{"root-token": true},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sys/seal-status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		sealed := f.sealed
		f.mu.Unlock()
		w.Write([]byte(`{"sealed":` + boolStr(sealed) + `}`))
	})
	mux.HandleFunc("/v1/auth/token/create", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct {
			Policies []string `json:"policies"`
			TTL      string   `json:"ttl"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.tokenSeq++
		tok := "child-token-" + itoa(f.tokenSeq)
		f.tokens[tok] = true
		w.Write([]byte(`{"auth":{"client_token":"` + tok + `"}}`))
	})
	mux.HandleFunc("/v1/secret/data/", f.handleData)
	mux.HandleFunc("/v1/secret/metadata/", f.handleMetadata)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// Tester is the subset of testing.T the fake needs.
type Tester interface {
	Cleanup(func())
}

// Addr is the fake's base URL.
func (f *Fake) Addr() string { return f.srv.URL }

// RootToken is the token the fixture accepts as root.
func (f *Fake) RootToken() string { return "root-token" }

// SetSealed flips the sealed state.
func (f *Fake) SetSealed(s bool) {
	f.mu.Lock()
	f.sealed = s
	f.mu.Unlock()
}

// Put seeds an entry directly (bypassing the HTTP API).
func (f *Fake) Put(path string, data map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := map[string]string{}
	for k, v := range data {
		cp[k] = v
	}
	f.store[path] = cp
	f.metadata[path] = true
}

// Exists reports whether an entry is present.
func (f *Fake) Exists(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metadata[path]
}

func (f *Fake) authed(w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	ok := f.tokens[r.Header.Get("X-Vault-Token")]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return false
	}
	return true
}

func (f *Fake) handleData(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		d, ok := f.store[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := json.Marshal(map[string]interface{}{
			"data": map[string]interface{}{
				"data": d,
				"metadata": map[string]interface{}{
					"version": 2, "created_time": "2026-01-01T00:00:00Z", "destroyed": false,
				},
			},
		})
		w.Write(b)
	case http.MethodPost:
		var req struct {
			Data map[string]string `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.store[path] = req.Data
		f.metadata[path] = true
		w.Write([]byte(`{"data":{"version":2}}`))
	case http.MethodDelete:
		delete(f.store, path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *Fake) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/secret/metadata/")
	switch r.Method {
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.metadata, path)
		delete(f.store, path)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Query().Get("list") != "true" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := strings.Trim(path, "/")
	var keys []string
	seen := map[string]bool{}
	for p := range f.metadata {
		if prefix != "" && !strings.HasPrefix(p, prefix+"/") {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i+1] // folder
		}
		if !seen[rest] {
			seen[rest] = true
			keys = append(keys, rest)
		}
	}
	if len(keys) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	b, _ := json.Marshal(map[string]interface{}{"data": map[string]interface{}{"keys": keys}})
	w.Write(b)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
