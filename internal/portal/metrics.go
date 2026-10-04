package portal

import (
	"log"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics for the portal HTTP surface. A private registry keeps the portal
// standalone from any controller-runtime metrics server.
type Metrics struct {
	Requests *prometheus.CounterVec
	Duration *prometheus.HistogramVec
	registry *prometheus.Registry
}

// NewMetrics builds the portal metric set with default process collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		prometheus.NewGoCollector())
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kubo_portal_requests_total",
			Help: "HTTP requests handled by the portal.",
		}, []string{"method", "path", "status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kubo_portal_request_duration_seconds",
			Help:    "Request latency by method and path.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "path"}),
		registry: reg,
	}
	reg.MustRegister(m.Requests, m.Duration)
	return m
}

// statusRecorder captures the response status for instrumentation.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying writer so streaming responses (SSE)
// work through the instrumented wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// instrument wraps a handler with request metrics and structured logging.
// Path templates (e.g. /api/stacks/{namespace}/{name}) are used as the label
// so cardinality stays bounded; unmatched paths fall back to the raw URL.
func (m *Metrics) instrument(pattern string, next http.Handler) http.Handler {
	label := pattern
	if label == "" {
		label = "unmatched"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		m.Requests.WithLabelValues(r.Method, label, http.StatusText(rec.status)).Inc()
		m.Duration.WithLabelValues(r.Method, label).Observe(time.Since(start).Seconds())
		log.Printf("portal: %s %s -> %d (%s)", r.Method, r.URL.Path,
			rec.status, time.Since(start).Round(time.Millisecond))
	})
}

// Handler serves the Prometheus /metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// observeRoute instruments a mux route registered with a pattern. ServeMux
// does not expose the matched pattern pre-Go 1.23 patterns via r.Pattern in
// 1.22; we instrument per-route instead by wrapping each registration.
func (p *Portal) handle(mux *http.ServeMux, m *Metrics, pattern string, h http.HandlerFunc) {
	if m == nil {
		mux.HandleFunc(pattern, h)
		return
	}
	mux.Handle(pattern, m.instrument(pattern, h))
}
