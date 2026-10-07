package agentfw

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── KillSwitch sources ──────────────────────────────────────────────────────

func TestKillSwitchEnvSource(t *testing.T) {
	t.Setenv("AGENTFW_KILL", "1")
	if !NewKillSwitch(filepath.Join(t.TempDir(), "ks")).Tripped() {
		t.Fatal("env source did not trip the kill switch")
	}
}

func TestKillSwitchFileSourceLatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "killswitch")
	ks := NewKillSwitch(path)
	if ks.Tripped() {
		t.Fatal("tripped before file exists")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !ks.Tripped() {
		t.Fatal("file source did not trip the kill switch")
	}
	// Documented behavior: tripping latches until the admin API resets it;
	// deleting the file alone does not re-enable traffic.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !ks.Tripped() {
		t.Fatal("kill switch unlatched after file removal; expected latch")
	}
}

// ── Signed audit receipts ───────────────────────────────────────────────────

func TestAuditorEmitsVerifiableSignedLine(t *testing.T) {
	dir := t.TempDir()
	signer, err := NewSigner(filepath.Join(dir, "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	a := NewAuditor(&buf).WithSigner(signer)
	a.Log(Event{Method: "CONNECT", URL: "api.anthropic.com:443", Action: "block",
		Findings: []Finding{{Kind: "killswitch", Pattern: "deny-all"}}})

	line := strings.TrimSpace(buf.String())
	var ev struct {
		Event
		Sig string `json:"sig"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.Sig == "" {
		t.Fatal("audit line is not signed")
	}
	if ev.ReceiptHash == "" {
		t.Fatal("audit line has no receipt hash")
	}
	// The signature covers the unsigned marshaling of the event.
	unsigned, _ := json.Marshal(ev.Event)
	if !Verify(signer.Pub, unsigned, ev.Sig) {
		t.Fatal("signature does not verify")
	}
	// Tampering must invalidate it.
	if Verify(signer.Pub, []byte(`{"tampered":true}`), ev.Sig) {
		t.Fatal("signature verified over tampered payload")
	}
}

func TestAuditorChainsReceipts(t *testing.T) {
	var buf strings.Builder
	a := NewAuditor(&buf)
	a.Log(Event{Action: "allow", URL: "https://one.example"})
	a.Log(Event{Action: "block", URL: "https://two.example"})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var first, second Event
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if second.PrevHash != first.ReceiptHash {
		t.Fatalf("chain mismatch: %q != %q", second.PrevHash, first.ReceiptHash)
	}
}

func TestSignerKeyReuse(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "signing.key")
	s1, err := NewSigner(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewSigner(keyPath) // second run loads the same key
	if err != nil {
		t.Fatal(err)
	}
	sig := s1.Sign([]byte("event"))
	if !Verify(s2.Pub, []byte("event"), sig) {
		t.Fatal("reloaded signer cannot verify signatures from persisted key")
	}
}

// ── Forward proxy plain-HTTP path ───────────────────────────────────────────

func TestProxyHandleHTTPForwardsAndAudits(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	// httptest upstream is on 127.0.0.1 — private-egress blocking must be off
	// for the happy path (it gets its own test below).
	p := DefaultPolicy()
	p.BlockPrivateEgress = false
	px := NewProxy(p, NewAuditor(&strings.Builder{}))

	target := strings.TrimPrefix(upstream.URL, "http://")
	req := httptest.NewRequest(http.MethodGet, "http://"+target+"/v1/models", nil)
	rec := httptest.NewRecorder()
	px.handleHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("clean forward failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyHandleHTTPBlocksPrivateEgress(t *testing.T) {
	p := DefaultPolicy() // BlockPrivateEgress: true
	px := NewProxy(p, NewAuditor(&strings.Builder{}))

	req := httptest.NewRequest(http.MethodGet, "http://192.168.1.1:8080/admin", nil)
	rec := httptest.NewRecorder()
	px.handleHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("private egress not blocked on forward path: %d", rec.Code)
	}
}

// ── MCP tool binding pinning ────────────────────────────────────────────────

func TestSessionBindToolsPinsAfterFirstUse(t *testing.T) {
	s := NewSessionStore()
	id := "sess-123"
	p := Policy{AllowedMCPTools: []string{"search", "calculator"}}

	if !s.BindTools(id, p, "search") {
		t.Fatal("first bind of allowed tool rejected")
	}
	// Same set still allowed...
	if !s.BindTools(id, p, "calculator") {
		t.Fatal("second allowed tool rejected")
	}
	// ...but a tool outside the first-bind set is now rejected (pinning).
	if s.BindTools(id, Policy{AllowedMCPTools: []string{"*"}}, "shell") {
		t.Fatal("tool set was widened after first bind; pinning violated")
	}
}

// ── Policy helpers ──────────────────────────────────────────────────────────

func TestPolicyMCPToolAllowed(t *testing.T) {
	wildcard := Policy{AllowedMCPTools: []string{"*"}}
	if !wildcard.MCPToolAllowed("anything") {
		t.Fatal("wildcard should allow all tools")
	}
	p := Policy{AllowedMCPTools: []string{"search"}}
	if !p.MCPToolAllowed("search") || p.MCPToolAllowed("shell") {
		t.Fatal("explicit allowlist mismatch")
	}
}

func TestLoadPolicyDefaultsOnMissingFile(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("missing policy file should report an error")
	}
	// But it must still fall back to safe defaults, not a zero-value policy.
	if !p.BlockPrivateEgress || p.DLPAction != "redact" || p.InjectionAction != "block" {
		t.Fatalf("unexpected defaults: %+v", p)
	}
}

// ── Signer rejects wrong keys ───────────────────────────────────────────────

func TestVerifyRejectsForeignSignature(t *testing.T) {
	s1, _ := NewSigner(filepath.Join(t.TempDir(), "k1"))
	s2, _ := NewSigner(filepath.Join(t.TempDir(), "k2"))
	data := []byte("event")
	var wrong ed25519.PublicKey = s2.Pub
	if Verify(wrong, data, s1.Sign(data)) {
		t.Fatal("signature from key A verified under key B")
	}
}
