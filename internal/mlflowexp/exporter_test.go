package mlflowexp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sampleMetrics = `# HELP agentfw_events_total Firewall audit events.
# TYPE agentfw_events_total counter
agentfw_events_total{action="block",kind="ssrf"} 3
agentfw_events_total{action="block",kind="killswitch"} 1
agentfw_events_total{action="redact",kind="dlp"} 7
agentfw_scanned_total 42
# HELP go_goroutines Number of goroutines.
# TYPE go_goroutines gauge
go_goroutines 9
`

func TestParseMetrics(t *testing.T) {
	m, err := ParseMetrics(sampleMetrics)
	if err != nil {
		t.Fatal(err)
	}
	if m[`agentfw_events_total{block,ssrf}`] != 3 {
		t.Fatalf("ssrf counter: %v", m)
	}
	if m["agentfw_scanned_total"] != 42 {
		t.Fatalf("scanned: %v", m)
	}
	if _, ok := m["go_goroutines"]; ok {
		t.Fatal("non-agentfw series leaked into parse")
	}
}

func TestParseMetricsEmpty(t *testing.T) {
	if m, err := ParseMetrics(""); err != nil || len(m) != 0 {
		t.Fatalf("empty parse: %v %v", m, err)
	}
}

func TestDiff(t *testing.T) {
	prev := map[string]float64{"a": 10, "b": 5}
	cur := map[string]float64{"a": 12, "b": 5, "c": 1}
	d := Diff(prev, cur)
	if d["a"] != 2 || d["c"] != 1 {
		t.Fatalf("unexpected deltas: %v", d)
	}
	if _, ok := d["b"]; ok {
		t.Fatal("zero delta should be omitted")
	}
}

func TestMetricKey(t *testing.T) {
	cases := map[string]string{
		`agentfw_events_total{block,ssrf}`: "events.block.ssrf",
		"agentfw_scanned_total":            "scanned",
		`agentfw_events_total{allow,none}`: "events.allow.none",
	}
	for in, want := range cases {
		if got := MetricKey(in); got != want {
			t.Fatalf("MetricKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPushAgainstMLflowStub(t *testing.T) {
	var gotRun string
	var gotMetrics []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/api/2.0/mlflow/experiments/get-by-name":
			w.Write([]byte(`{"experiment_id":"123"}`))
		case "/api/2.0/mlflow/runs/create":
			w.Write([]byte(`{"run":{"info":{"run_id":"run-1"}}}`))
		case "/api/2.0/mlflow/runs/log-batch":
			gotRun, _ = body["run_id"].(string)
			if raw, ok := body["metrics"].([]any); ok {
				for _, m := range raw {
					if mm, ok := m.(map[string]any); ok {
						gotMetrics = append(gotMetrics, mm)
					}
				}
			}
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "agentfw-security", "fw")
	err := c.Push(map[string]float64{
		`agentfw_events_total{block,dlp}`: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.runID != "run-1" || gotRun != "run-1" {
		t.Fatalf("run not created/used: %q %q", c.runID, gotRun)
	}
	if len(gotMetrics) != 1 {
		t.Fatalf("expected 1 metric, got %v", gotMetrics)
	}
	if gotMetrics[0]["key"] != "events.block.dlp" {
		t.Fatalf("metric key: %v", gotMetrics[0]["key"])
	}
}

func TestPushEmptyIsNoop(t *testing.T) {
	// No MLflow server at all — must not attempt a connection.
	c := NewClient("http://127.0.0.1:1", "x", "y")
	if err := c.Push(nil); err != nil {
		t.Fatalf("empty push should be a noop, got: %v", err)
	}
}

func TestPollerFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(sampleMetrics))
	}))
	defer srv.Close()

	m, err := NewPoller(srv.URL).Fetch()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(keys(m), ","), "agentfw_scanned_total") {
		t.Fatalf("fetched: %v", m)
	}
}

func keys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
