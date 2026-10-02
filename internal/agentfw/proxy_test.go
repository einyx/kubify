package agentfw

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

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
