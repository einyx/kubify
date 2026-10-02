// Package mlflowexp pushes agentfw Prometheus counters into MLflow as
// experiment metrics — derived aggregates only, never raw audit events.
package mlflowexp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// Client talks to the MLflow REST API.
type Client struct {
	BaseURL    string // e.g. http://mlflow:5000
	Experiment string
	RunName    string
	HTTP       *http.Client
	runID      string
}

// NewClient resolves (or creates) the experiment and run lazily on first push.
func NewClient(baseURL, experiment, runName string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		Experiment: experiment,
		RunName:    runName,
		HTTP:       &http.Client{Timeout: 15 * time.Second},
	}
}

// Poller fetches the agentfw Prometheus exposition endpoint.
type Poller struct {
	URL  string
	HTTP *http.Client
}

// NewPoller targets an agentfw admin endpoint (e.g. http://agentfw:8081).
func NewPoller(adminURL string) *Poller {
	return &Poller{URL: strings.TrimRight(adminURL, "/") + "/metrics", HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Fetch returns current cumulative agentfw counter values.
func (p *Poller) Fetch() (map[string]float64, error) {
	resp, err := p.HTTP.Get(p.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("agentfw metrics: %s", resp.Status)
	}
	text, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return ParseMetrics(string(text))
}

// ParseMetrics extracts agentfw_* sample series from Prometheus text format.
// Returns map["action/kind"]value.
func ParseMetrics(text string) (map[string]float64, error) {
	// agentfw names are legacy underscore-style; the zero-value parser's
	// scheme is unset (invalid), so construct explicitly.
	var parser = expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		return nil, fmt.Errorf("parse metrics: %w", err)
	}
	out := map[string]float64{}
	for name, fam := range families {
		if !strings.HasPrefix(name, "agentfw_") {
			continue
		}
		for _, m := range fam.GetMetric() {
			key := name
			if labels := m.GetLabel(); len(labels) > 0 {
				parts := make([]string, 0, len(labels))
				for _, l := range labels {
					parts = append(parts, l.GetValue())
				}
				sort.Strings(parts)
				key = name + "{" + strings.Join(parts, ",") + "}"
			}
			out[key] += sampleValue(m)
		}
	}
	return out, nil
}

// sampleValue reads a sample regardless of how the parser typed the family
// (legacy text input can come back as UNTYPED).
func sampleValue(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if g := m.GetGauge(); g != nil {
		return g.GetValue()
	}
	return m.GetUntyped().GetValue()
}

// Diff returns the per-interval deltas from cumulative counters.
func Diff(prev, cur map[string]float64) map[string]float64 {
	d := map[string]float64{}
	for k, v := range cur {
		delta := v - prev[k]
		if delta > 0 {
			d[k] = delta
		}
	}
	return d
}

// MetricKey converts "agentfw_events_total{action,block,kind,dlp}" into an
// MLflow metric name like "agentfw.block.dlp".
func MetricKey(series string) string {
	name, labels := series, ""
	if i := strings.Index(series, "{"); i >= 0 {
		name = series[:i]
		labels = series[i+1 : len(series)-1]
	}
	name = strings.TrimPrefix(name, "agentfw_")
	name = strings.TrimSuffix(name, "_total")
	parts := []string{name}
	for _, kv := range strings.Split(labels, ",") {
		if kv == "" {
			continue
		}
		parts = append(parts, kv[strings.Index(kv, "=")+1:])
	}
	return strings.Join(parts, ".")
}

// Push logs the given metric deltas into the run.
func (c *Client) Push(deltas map[string]float64) error {
	if len(deltas) == 0 {
		return nil
	}
	if c.runID == "" {
		if err := c.ensureRun(); err != nil {
			return err
		}
	}
	now := time.Now().UnixMilli()
	var metrics []map[string]any
	for k, v := range deltas {
		metrics = append(metrics, map[string]any{
			"key":       MetricKey(k),
			"value":     v,
			"timestamp": now,
			"step":      now / 1000,
		})
	}
	return c.post("/api/2.0/mlflow/runs/log-batch", map[string]any{
		"run_id":  c.runID,
		"metrics": metrics,
	})
}

func (c *Client) ensureRun() error {
	// Create-or-get experiment by name.
	var exp struct {
		ExperimentID string `json:"experiment_id"`
		ErrorCode    string `json:"error_code"`
		Message      string `json:"message"`
	}
	err := c.postParse("/api/2.0/mlflow/experiments/get-by-name", map[string]any{"experiment_name": c.Experiment}, &exp)
	if err != nil {
		if exp.ErrorCode != "RESOURCE_DOES_NOT_EXIST" {
			return fmt.Errorf("mlflow experiment lookup: %w", err)
		}
		if err := c.postParse("/api/2.0/mlflow/experiments/create", map[string]any{"name": c.Experiment}, &exp); err != nil {
			return fmt.Errorf("mlflow experiment create: %w", err)
		}
	}
	var run struct {
		Run struct {
			Info struct {
				RunID string `json:"run_id"`
			} `json:"info"`
		} `json:"run"`
	}
	err = c.postParse("/api/2.0/mlflow/runs/create", map[string]any{
		"experiment_id": exp.ExperimentID,
		"run_name":      c.RunName,
	}, &run)
	if err != nil {
		return fmt.Errorf("mlflow run create: %w", err)
	}
	c.runID = run.Run.Info.RunID
	return nil
}

func (c *Client) post(path string, body any) error {
	return c.postParse(path, body, nil)
}

func (c *Client) postParse(path string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(http.MethodPost, c.BaseURL+path, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("mlflow %s: %s %s", path, resp.Status, e.Message)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// FormatValue keeps MLflow metric values tidy (no float noise).
func FormatValue(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
