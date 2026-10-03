package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentfwProxyDisabled(t *testing.T) {
	p := New(nil)
	mux := p.Mux()

	if p.AgentfwEnabled() {
		t.Fatal("agentfw should be disabled by default")
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /agentfw/ without config = %d, want 404", w.Code)
	}
	if html := p.GetIndexHTML(); strings.Contains(html, `href="/agentfw/"`) {
		t.Fatal("nav link must be absent when agentfw is disabled")
	}
}

func TestAgentfwProxySurfacesArchive(t *testing.T) {
	// Stand-in for the agentfw admin port.
	var gotPath string
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"total_requests":7}`))
	}))
	defer es.Close()

	p := New(nil)
	if err := p.SetAgentfwURL(es.URL); err != nil {
		t.Fatalf("SetAgentfwURL: %v", err)
	}
	if !p.AgentfwEnabled() {
		t.Fatal("agentfw should be enabled after SetAgentfwURL")
	}
	mux := p.Mux()

	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/api/v1/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /agentfw/api/v1/stats = %d, body: %s", w.Code, w.Body.String())
	}
	if gotPath != "/api/v1/stats" {
		t.Fatalf("upstream path = %q, want /api/v1/stats (mount prefix must be stripped)", gotPath)
	}
	if !strings.Contains(w.Body.String(), `"total_requests":7`) {
		t.Fatalf("proxied body = %s", w.Body.String())
	}

	// HTML responses get the SPA base injected so the viewer's API calls
	// resolve under /agentfw/.
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body><script>let x=1;</script></body></html>"))
	}))
	defer html.Close()
	if err := p.SetAgentfwURL(html.URL); err != nil {
		t.Fatalf("SetAgentfwURL: %v", err)
	}
	r3 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/", nil)
	w3 := httptest.NewRecorder()
	p.Mux().ServeHTTP(w3, r3)
	if !strings.Contains(w3.Body.String(), `window.__AFW_BASE__="/agentfw"`) {
		t.Fatalf("base not injected into proxied HTML: %s", w3.Body.String())
	}

	// Nav link appears in the served UI.
	r2 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if !strings.Contains(w2.Body.String(), `href="/agentfw/"`) {
		t.Fatal("nav link missing from served index when agentfw is enabled")
	}
}

func TestSetAgentfwURLValidation(t *testing.T) {
	p := New(nil)
	if err := p.SetAgentfwURL("ftp://nope"); err == nil {
		t.Fatal("expected error for non-http scheme")
	}
	if err := p.SetAgentfwURL(""); err != nil {
		t.Fatalf("empty url should disable, got %v", err)
	}
	if p.AgentfwEnabled() {
		t.Fatal("agentfw should be disabled after empty SetAgentfwURL")
	}
}
