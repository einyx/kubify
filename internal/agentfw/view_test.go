package agentfw

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseUsageOpenAI(t *testing.T) {
	body := `{"model":"gpt-4o","usage":{"prompt_tokens":1000,"completion_tokens":250,"prompt_tokens_details":{"cached_tokens":200}}}`
	u := ParseUsage(body)
	if !u.HasUsage {
		t.Fatal("expected usage")
	}
	if u.Model != "gpt-4o" || u.InputTokens != 1000 || u.OutputTokens != 250 || u.CacheRead != 200 {
		t.Fatalf("bad usage: %+v", u)
	}
	// gpt-4o: $2.50 in / $10 out per 1M → 1000*2.5 + 250*10 micro = 2500+2500 = 5000
	if u.CostMicro != 5000 {
		t.Fatalf("cost = %d, want 5000", u.CostMicro)
	}
}

func TestParseUsageAnthropic(t *testing.T) {
	body := `{"model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":100,"output_tokens":1000}}`
	u := ParseUsage(body)
	if !u.HasUsage || u.InputTokens != 100 || u.OutputTokens != 1000 {
		t.Fatalf("bad usage: %+v", u)
	}
	// claude-3-5-sonnet family: $3 in / $15 out → 300 + 15000 = 15300
	if u.CostMicro != 15300 {
		t.Fatalf("cost = %d, want 15300", u.CostMicro)
	}
}

func TestParseUsageGoogle(t *testing.T) {
	body := `{"model":"gemini-2.0-flash","usageMetadata":{"promptTokenCount":500,"candidatesTokenCount":100}}`
	u := ParseUsage(body)
	if !u.HasUsage || u.InputTokens != 500 || u.OutputTokens != 100 {
		t.Fatalf("bad usage: %+v", u)
	}
}

func TestParseUsageNonLLM(t *testing.T) {
	if u := ParseUsage(`<html>not json</html>`); u.HasUsage {
		t.Fatal("expected no usage for non-JSON")
	}
	if u := ParseUsage(`{"foo":"bar"}`); u.HasUsage {
		t.Fatal("expected no usage for body without usage")
	}
}

func TestRequestModel(t *testing.T) {
	if m := RequestModel(`{"model":"o3-mini","messages":[]}`); m != "o3-mini" {
		t.Fatalf("model = %q", m)
	}
	if m := RequestModel(`not json`); m != "" {
		t.Fatalf("model = %q, want empty", m)
	}
}

func newTestArchive(t *testing.T) *Archive {
	t.Helper()
	a, err := OpenArchive(filepath.Join(t.TempDir(), "view.db"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func TestArchiveInsertListGet(t *testing.T) {
	a := newTestArchive(t)

	id, err := a.Insert(Record{
		SessionID: "ip:10.0.0.1", Method: "POST", URL: "https://api.openai.com/v1/chat/completions",
		Host: "api.openai.com", Model: "gpt-4o", Status: 200, DurationMS: 120,
		ReqBytes: 10, RespBytes: 20, Action: "allow",
		ReqBody:  `{"model":"gpt-4o","messages":[{"role":"user","content":"hello secret-plan-xyz"}]}`,
		RespBody: `{"model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		Findings: []Finding{{Kind: "dlp", Pattern: "aws-key", Excerpt: "redacted"}},
		Usage:    &Usage{Model: "gpt-4o", InputTokens: 10, OutputTokens: 5, CostMicro: 60, HasUsage: true},
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := a.Insert(Record{SessionID: "ip:10.0.0.2", Method: "POST", URL: "https://x.local/y", Host: "x.local", Action: "block", Status: 403}); err != nil {
		t.Fatalf("insert 2: %v", err)
	}

	recs, err := a.List(SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}

	// Action filter
	blocks, err := a.List(SearchOptions{Action: "block"})
	if err != nil || len(blocks) != 1 || blocks[0].Action != "block" {
		t.Fatalf("block filter: %v %+v", err, blocks)
	}

	// Session filter
	sess, err := a.List(SearchOptions{SessionID: "ip:10.0.0.1"})
	if err != nil || len(sess) != 1 || sess[0].ID != id {
		t.Fatalf("session filter: %v %+v", err, sess)
	}

	// Get with bodies and findings
	full, err := a.Get(id)
	if err != nil || full == nil {
		t.Fatalf("get: %v", err)
	}
	if full.ReqBody == "" || len(full.Findings) != 1 || full.Usage == nil || full.Usage.CostMicro != 60 {
		t.Fatalf("incomplete record: %+v", full)
	}
}

func TestArchiveSearch(t *testing.T) {
	a := newTestArchive(t)
	if _, err := a.Insert(Record{SessionID: "s1", Method: "POST", URL: "https://api.openai.com/v1/x", Host: "api.openai.com", Action: "allow", ReqBody: `{"content":"the launch codes are 1-2-3"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Insert(Record{SessionID: "s2", Method: "POST", URL: "https://anthropic.com/v1/y", Host: "anthropic.com", Action: "allow", ReqBody: `{"content":"weather today"}`}); err != nil {
		t.Fatal(err)
	}

	recs, err := a.List(SearchOptions{Query: "launch codes"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("search hits = %d, want 1", len(recs))
	}

	total, err := a.SearchCount(SearchOptions{Query: "launch codes"})
	if err != nil || total != 1 {
		t.Fatalf("count = %d err=%v", total, err)
	}
}

func TestArchiveSessionsAndStats(t *testing.T) {
	a := newTestArchive(t)
	now := time.Now()
	mk := func(sid, model, action string, in, out int64, at time.Time) {
		t.Helper()
		_, err := a.Insert(Record{
			SessionID: sid, Method: "POST", URL: "https://api.openai.com/v1/x", Host: "api.openai.com",
			Model: model, Action: action, Status: 200, Time: at,
			Usage: &Usage{Model: model, InputTokens: in, OutputTokens: out, CostMicro: in + out, HasUsage: true},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	mk("s1", "gpt-4o", "allow", 100, 50, now.Add(-2*time.Minute))
	mk("s1", "gpt-4o", "block", 10, 5, now.Add(-time.Minute))
	mk("s2", "claude-3-5-haiku", "redact", 200, 20, now)

	sessions, err := a.Sessions(10)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("sessions: %v %+v", err, sessions)
	}
	if sessions[0].SessionID != "s2" { // most recent first
		t.Fatalf("expected s2 first, got %s", sessions[0].SessionID)
	}
	s1 := sessions[1]
	if s1.Requests != 2 || s1.Blocked != 1 || s1.InputTokens != 110 || s1.CostMicro != 165 {
		t.Fatalf("s1 rollup wrong: %+v", s1)
	}

	stats, err := a.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalRequests != 3 || stats.Blocked != 1 || stats.Redacted != 1 || stats.TotalSessions != 2 {
		t.Fatalf("stats wrong: %+v", stats)
	}
	if len(stats.TopModels) != 2 {
		t.Fatalf("top models = %d", len(stats.TopModels))
	}
}

func TestViewerEndpoints(t *testing.T) {
	a := newTestArchive(t)
	a.Insert(Record{SessionID: "ip:1", Method: "POST", URL: "https://api.openai.com/v1/x", Host: "api.openai.com", Action: "allow", ReqBody: `{"q":"findme-needle"}`}) //nolint:errcheck

	v := NewViewer()
	v.SetArchive(a)
	srv := httptest.NewServer(v.Handler())
	defer srv.Close()

	get := func(path string) map[string]any {
		t.Helper()
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("GET %s = %d", path, r.StatusCode)
		}
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return m
	}

	if m := get("/api/v1/healthz"); m["archive"] != true {
		t.Fatalf("healthz: %v", m)
	}
	if m := get("/api/v1/stats"); m["total_requests"].(float64) != 1 {
		t.Fatalf("stats: %v", m)
	}
	if m := get("/api/v1/sessions"); len(m["sessions"].([]any)) != 1 {
		t.Fatalf("sessions: %v", m)
	}
	if m := get(`/api/v1/requests?q=findme-needle`); len(m["requests"].([]any)) != 1 {
		t.Fatalf("search api: %v", m)
	}
	if m := get(`/api/v1/requests?action=block`); len(m["requests"].([]any)) != 0 {
		t.Fatalf("action filter: %v", m)
	}

	// UI serves HTML
	r, err := http.Get(srv.URL + "/")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("UI: %v %d", err, r.StatusCode)
	}
	r.Body.Close()
}

func TestViewerWithoutArchiveIs503(t *testing.T) {
	v := NewViewer()
	srv := httptest.NewServer(v.Handler())
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without archive, got %d", r.StatusCode)
	}
}

func TestViewerBasePath(t *testing.T) {
	a := newTestArchive(t)
	v := WithBasePath("/agentfw")
	v.SetArchive(a)
	srv := httptest.NewServer(v.Handler())
	defer srv.Close()

	// Prefixed routes work.
	r, err := http.Get(srv.URL + "/agentfw/api/v1/stats")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("prefixed stats: %v %d", err, r.StatusCode)
	}
	r.Body.Close()

	// Bare routes do not exist under a base path.
	r, err = http.Get(srv.URL + "/api/v1/stats")
	if err != nil || r.StatusCode != 404 {
		t.Fatalf("bare stats under base path = %d, want 404", r.StatusCode)
	}
	r.Body.Close()

	// UI is served at the prefix and injects the base for the SPA.
	r, err = http.Get(srv.URL + "/agentfw/")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("prefixed UI: %v %d", err, r.StatusCode)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), `window.__AFW_BASE__="/agentfw"`) {
		t.Fatal("UI does not inject the base path")
	}
}

func TestFTSQuerySanitized(t *testing.T) {
	// Operator/quote injection must not break the query.
	got := ftsQuery(`hello "world" (test): *`)
	want := `"hello"* "world"* "test"*`
	if got != want {
		t.Fatalf("ftsQuery = %q, want %q", got, want)
	}
}
