package agentfw

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// ── Kill switch admin API ───────────────────────────────────────────────────

func TestKillSwitchAdminHandlerLifecycle(t *testing.T) {
	ks := NewKillSwitch(filepath.Join(t.TempDir(), "killswitch"))
	h := ks.AdminHandler()

	do := func(method string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/kill", nil))
		return rec
	}

	if rec := do(http.MethodGet); !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("initial status: %s", rec.Body.String())
	}

	if rec := do(http.MethodPost); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "tripped") {
		t.Fatalf("POST /kill: %d %s", rec.Code, rec.Body.String())
	}
	if !ks.Tripped() {
		t.Fatal("POST did not trip the switch")
	}
	if rec := do(http.MethodGet); !strings.Contains(rec.Body.String(), "tripped") {
		t.Fatalf("status after trip: %s", rec.Body.String())
	}

	if rec := do(http.MethodDelete); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "reset") {
		t.Fatalf("DELETE /kill: %d %s", rec.Code, rec.Body.String())
	}
	if ks.Tripped() {
		t.Fatal("DELETE did not reset the switch")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/kill", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /kill: %d", rec.Code)
	}
}

// ── Rate limiter middleware ─────────────────────────────────────────────────

func TestRateLimiterMiddleware429sAndAudits(t *testing.T) {
	// burst = rpm/10 (min 1): rpm=2 means one immediate request, then 429.
	rl := NewRateLimiter(2, 0)
	var buf strings.Builder
	auditor := NewAuditor(&buf)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := rl.Middleware(next, auditor)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("burst request blocked under limit: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("{}")))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit request not 429: %d", rec.Code)
	}
	if !strings.Contains(buf.String(), "rate limit exceeded") {
		t.Fatalf("429 not audited: %q", buf.String())
	}
}

func TestRateLimiterDataBudgetCountsBodies(t *testing.T) {
	// 1 MB budget; a 2 MB body must trip it even with unlimited rate.
	rl := NewRateLimiter(0, 1)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), NewAuditor(&strings.Builder{}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/upload",
		strings.NewReader(strings.Repeat("x", 2<<20))))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget request not blocked: %d", rec.Code)
	}
}

// ── Session ID extraction ───────────────────────────────────────────────────

func TestSessionIDPriority(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://api.example.com/v1/x", nil)
	r.RemoteAddr = "10.0.0.9:5555"

	if got := SessionID(r); got != "ip:10.0.0.9:5555" {
		t.Fatalf("ip fallback: %q", got)
	}
	r.AddCookie(&http.Cookie{Name: "session", Value: "abc"})
	if got := SessionID(r); got != "cookie:abc" {
		t.Fatalf("cookie: %q", got)
	}
	r.Header.Set("X-Session-ID", "sess-9")
	if got := SessionID(r); got != "sess-9" {
		t.Fatalf("header: %q", got)
	}
}

// ── Reverse proxy mode (upstream set) ───────────────────────────────────────

func TestReverseProxyForwardsAndRedactsResponses(t *testing.T) {
	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		// A secret the LLM echoed back must be redacted on the way out.
		w.Write([]byte(`leak: AKIAIOSFODNN7EXAMPLE`))
	}))
	defer upstream.Close()

	p := DefaultPolicy() // dlpAction: redact
	rv, err := NewReverseProxy(upstream.URL, p, NewAuditor(&strings.Builder{}))
	if err != nil {
		t.Fatal(err)
	}

	// Client thinks it is talking to the LLM API; the proxy rewrites upstream.
	req := httptest.NewRequest(http.MethodPost, "http://api.openai.com/v1/chat", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	rv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("reverse proxy status: %d %s", rec.Code, rec.Body.String())
	}
	if seenPath != "/v1/chat" {
		t.Fatalf("path not preserved: %q", seenPath)
	}
	if strings.Contains(rec.Body.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("secret leaked through reverse proxy unredacted")
	}
	if !strings.Contains(rec.Body.String(), "REDACTED") {
		t.Fatalf("expected redaction marker, got: %q", rec.Body.String())
	}
}

func TestReverseProxyRejectsInvalidUpstream(t *testing.T) {
	if _, err := NewReverseProxy("://bad", DefaultPolicy(), nil); err == nil {
		t.Fatal("invalid upstream accepted")
	}
}

// ── Receipt tamper evidence end-to-end ──────────────────────────────────────

func TestAuditLineTamperDetection(t *testing.T) {
	dir := t.TempDir()
	signer, err := NewSigner(filepath.Join(dir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	NewAuditor(&buf).WithSigner(signer).Log(Event{Action: "allow", URL: "https://x.y"})

	var rec struct {
		Event
		Sig string `json:"sig"`
	}
	line := strings.TrimSpace(buf.String())
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	unsigned, _ := json.Marshal(rec.Event)

	// Flipping one bit of the event must invalidate the signature.
	tampered := append([]byte(nil), unsigned...)
	tampered[len(tampered)-2] ^= 0x01
	var wrong ed25519.PublicKey = signer.Pub
	if Verify(wrong, tampered, rec.Sig) {
		t.Fatal("tampered event verified")
	}
}
