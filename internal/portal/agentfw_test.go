package portal

import (
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"net/http"
)

func TestAgentfwProxyDisabled(t *testing.T) {
	p := New(nil)
	mux := p.Mux()

	if p.AgentfwEnabled() {
		t.Fatal("agentfw should be disabled by default")
	}
	// Bookmarks always land in the SPA view, configured or not.
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "/#/agents" {
		t.Fatalf("GET /agentfw/ without config = %d %q, want 308 /#/agents", w.Code, w.Header().Get("Location"))
	}
	// The API itself stays 404 until an instance is configured or discovered.
	rAPI := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/api/v1/stats", nil)
	wAPI := httptest.NewRecorder()
	mux.ServeHTTP(wAPI, rAPI)
	if wAPI.Code != http.StatusNotFound {
		t.Fatalf("GET /agentfw/api/v1/stats without config = %d, want 404", wAPI.Code)
	}
	if html := p.GetIndexHTML(); strings.Contains(html, `onclick="showAgents()"`) {
		t.Fatal("agent view button must be absent when agentfw is disabled")
	}
}

func TestAgentfwSessionIP(t *testing.T) {
	tests := map[string]string{
		"ip:10.110.1.234:43122":  "10.110.1.234",
		"ip:[2001:db8::1]:43122": "2001:db8::1",
		"application-session":    "",
	}
	for sessionID, want := range tests {
		if got := agentfwSessionIP(sessionID); got != want {
			t.Errorf("agentfwSessionIP(%q) = %q, want %q", sessionID, got, want)
		}
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

	// The standalone page is gone: /agentfw/ redirects into the SPA view.
	r2 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code != http.StatusPermanentRedirect || w2.Header().Get("Location") != "/#/agents" {
		t.Fatalf("GET /agentfw/ = %d %q, want 308 /#/agents", w2.Code, w2.Header().Get("Location"))
	}

	// Nav button appears in the served UI and opens the in-SPA view.
	r3 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, r3)
	if !strings.Contains(w3.Body.String(), `onclick="showAgents()"`) {
		t.Fatal("agent view button missing from served index when agentfw is enabled")
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

func TestSetAgentfwServiceProxy(t *testing.T) {
	p := &Portal{restCfg: &rest.Config{Host: "https://api.example.com"}}
	if err := p.SetAgentfwURL("svc:foundation-a/agentfw"); err != nil {
		t.Fatal(err)
	}
	if !p.AgentfwEnabled() || p.agentfwURL != "svc:foundation-a/agentfw" {
		t.Fatalf("agentfwURL = %q", p.agentfwURL)
	}
	// invalid spec rejected
	if err := p.SetAgentfwURL("svc:noslash"); err == nil {
		t.Error("expected error for svc spec without namespace/service")
	}
	// empty disables
	if err := p.SetAgentfwURL(""); err != nil {
		t.Fatal(err)
	}
	if p.AgentfwEnabled() {
		t.Error("empty URL should disable the integration")
	}
}
