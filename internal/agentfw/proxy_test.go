package agentfw

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// Base-URL mode: an origin-form request (client set e.g.
// ANTHROPIC_BASE_URL=http://agentfw:8080) must resolve an upstream via the
// policy default instead of failing with "http: no Host in request URL".
func TestProxyBaseURLDefaultResolvesTarget(t *testing.T) {
	p := DefaultPolicy()
	p.BaseURLDefault = "https://api.anthropic.com"
	var buf strings.Builder
	px := NewProxy(p, NewAuditor(&buf))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err := px.resolveTarget(req); err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if req.URL.Host != "api.anthropic.com" || req.URL.Scheme != "https" || req.Host != "api.anthropic.com" {
		t.Fatalf("bad target: %s://%s host=%q", req.URL.Scheme, req.URL.Host, req.Host)
	}
	if req.URL.Path != "/v1/messages" {
		t.Fatalf("path rewritten: %q", req.URL.Path)
	}
}

func TestProxyBaseURLResolutionOrder(t *testing.T) {
	tests := []struct {
		name    string
		policy  func(*Policy)
		header  string
		path    string
		wantHdr string // "x-agentfw-upstream" value after resolve
		want    string // scheme://host/path
	}{
		{
			name:    "header wins over policy",
			policy:  func(p *Policy) { p.BaseURLDefault = "https://api.anthropic.com" },
			header:  "api.openai.com",
			path:    "/v1/chat/completions",
			wantHdr: "",
			want:    "https://api.openai.com/v1/chat/completions",
		},
		{
			name:   "header accepts full base URL with path",
			policy: func(p *Policy) {},
			header: "https://gateway.internal/llm",
			path:   "/v1/messages",
			want:   "https://gateway.internal/llm/v1/messages",
		},
		{
			name:   "path-embedded target",
			policy: func(p *Policy) {},
			path:   "/https://api.openai.com/v1/chat/completions",
			want:   "https://api.openai.com/v1/chat/completions",
		},
		{
			name:   "host route with port stripped",
			policy: func(p *Policy) { p.BaseURLRoutes = map[string]string{"agentfw": "https://llm.internal"} },
			path:   "/v1/messages",
			want:   "https://llm.internal/v1/messages",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := DefaultPolicy()
			if tt.policy != nil {
				tt.policy(&p)
			}
			px := NewProxy(p, NewAuditor(&strings.Builder{}))
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req.Host = "agentfw:8080"
			if tt.header != "" {
				req.Header.Set(upstreamHeader, tt.header)
			}
			if err := px.resolveTarget(req); err != nil {
				t.Fatalf("resolveTarget: %v", err)
			}
			if got := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path; got != tt.want {
				t.Fatalf("target = %q, want %q", got, tt.want)
			}
			if got := req.Header.Get(upstreamHeader); got != tt.wantHdr {
				t.Fatalf("upstream header = %q, want %q", got, tt.wantHdr)
			}
		})
	}
}

// Unresolvable origin-form requests must fail with a clear 400, not the
// opaque "http: no Host in request URL" from the reverse proxy.
func TestProxyBaseURLNoTarget(t *testing.T) {
	px := NewProxy(DefaultPolicy(), NewAuditor(&strings.Builder{}))
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	if err := px.resolveTarget(req); err == nil {
		t.Fatal("expected error for unresolvable target")
	}
	rec := httptest.NewRecorder()
	px.handleHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "baseURLDefault") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

// Base-URL mode must not weaken the SSRF floor: a default pointing at a
// private address is still blocked by the scanner pipeline.
func TestProxyBaseURLPrivateEgressBlocked(t *testing.T) {
	p := DefaultPolicy()
	p.BaseURLDefault = "http://10.0.0.5:9000"
	var buf strings.Builder
	px := NewProxy(p, NewAuditor(&buf))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	px.handleHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (private egress)", rec.Code)
	}
	if !strings.Contains(buf.String(), `"kind":"ssrf"`) {
		t.Fatalf("expected ssrf finding, got: %q", buf.String())
	}
}

// A killed proxy must refuse CONNECT tunnels even when MITM is off —
// otherwise the kill switch is a no-op for HTTPS egress.
func TestProxyKillSwitchBlocksTunnel(t *testing.T) {
	dir := t.TempDir()
	ks := NewKillSwitch(filepath.Join(dir, "killswitch"))
	p := DefaultPolicy()
	var buf strings.Builder
	a := NewAuditor(&buf)
	px := NewProxy(p, a)
	px.scanner.KillSwitch = ks

	// Not tripped: tunnel proceeds past the kill-switch gate (it may still
	// fail later on dial/DNS in this environment — only a killswitch finding
	// would be wrong here).
	req := httptest.NewRequest(http.MethodConnect, "http://api.anthropic.com:443", nil)
	req.URL.Host = "api.anthropic.com:443"
	rec := httptest.NewRecorder()
	px.handleTunnel(rec, req)
	if strings.Contains(buf.String(), `"kind":"killswitch"`) {
		t.Fatalf("killswitch tripped without being armed: %q", buf.String())
	}

	// Trip via file source, hot path must pick it up.
	ks.Trip()
	buf.Reset()
	rec = httptest.NewRecorder()
	px.handleTunnel(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tunnel allowed while kill switch tripped: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "kill switch") {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"kind":"killswitch"`) {
		t.Fatalf("expected killswitch audit event, got: %q", buf.String())
	}
}
