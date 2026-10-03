package agentfw

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics for the firewall. Counters are incremented from
// Auditor.Log — the single choke point every block/redact/allow passes
// through — so metrics can never add per-request latency beyond an
// atomic increment and never fail a request when a collector is down.
var (
	eventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "agentfw_events_total",
		Help: "Firewall audit events by action and finding kind.",
	}, []string{"action", "kind"})
	scannedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "agentfw_scanned_total",
		Help: "Responses inspected by the scanner pipeline.",
	})
)

// observeEvent converts an audit event into metric increments.
func observeEvent(ev Event) {
	if len(ev.Findings) == 0 {
		eventsTotal.WithLabelValues(ev.Action, "none").Inc()
		return
	}
	seen := map[string]bool{}
	for _, f := range ev.Findings {
		if !seen[f.Kind] {
			eventsTotal.WithLabelValues(ev.Action, f.Kind).Inc()
			seen[f.Kind] = true
		}
	}
}

// MetricsHandler serves the Prometheus exposition format on the admin port.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

// adminMux mounts the kill-switch API, /metrics, and the viewer (archive
// UI + API) on one admin listener. The viewer mux's "/" route is the
// least-specific pattern, so /kill and /metrics keep precedence.
func adminMux(ks *KillSwitch, viewer *Viewer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/kill", ks.AdminHandler())
	mux.Handle("/metrics", MetricsHandler())
	mux.Handle("/", viewer.Handler())
	return mux
}
