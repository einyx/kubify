package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// inClusterPortal builds a portal whose outOfCluster flag is false (as it
// would be inside a pod) with a fake client seeded with the given services.
func inClusterPortal(t *testing.T, svcs ...*corev1.Service) *Portal {
	t.Helper()
	p := newFake(t, svcObjs(svcs)...)
	p.outOfCluster = false
	return p
}

func svcObjs(svcs []*corev1.Service) []client.Object {
	out := make([]client.Object, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, s)
	}
	return out
}

func agentfwSvc(ns string, ports ...corev1.ServicePort) *corev1.Service {
	if len(ports) == 0 {
		ports = []corev1.ServicePort{{Name: "admin", Port: 8081}}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "agentfw", Namespace: ns},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

// In-cluster discovery must register a direct service-DNS fetcher (no
// kubectl port-forward child), visible in the host of the fetch error.
// shortCtx fails a fetch fast on hosts where cluster DNS names hang instead
// of erroring immediately; the error still names the target host.
func shortCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestAgentfwFetcherInClusterHitsServiceDNS(t *testing.T) {
	p := inClusterPortal(t)
	fetch := p.agentfwFetcher("stack-a", 0)
	_, err := fetch(shortCtx(t), "/api/v1/stats")
	if err == nil {
		t.Fatal("fetch against a nonexistent cluster DNS name should fail")
	}
	if !strings.Contains(err.Error(), "agentfw.stack-a.svc.cluster.local:8081") {
		t.Fatalf("fetch must target the service DNS, got error: %v", err)
	}
}

// A custom admin port must be honored.
func TestAgentfwFetcherInClusterCustomPort(t *testing.T) {
	p := inClusterPortal(t)
	fetch := p.agentfwFetcher("stack-b", 9099)
	_, err := fetch(shortCtx(t), "/api/v1/stats")
	if err == nil || !strings.Contains(err.Error(), ":9099") {
		t.Fatalf("custom port not honored: %v", err)
	}
}

// The svc: explicit spec must use the same in-cluster direct path.
func TestSetAgentfwURLSvcSpecInCluster(t *testing.T) {
	p := inClusterPortal(t)
	if err := p.SetAgentfwURL("svc:stack-a/agentfw"); err != nil {
		t.Fatal(err)
	}
	items := p.agentfwSnapshot()
	if len(items) != 1 || items[0].Label != "stack-a" || !items[0].Explicit {
		t.Fatalf("items = %+v", items)
	}
	_, err := items[0].Fetch(shortCtx(t), "/api/v1/stats")
	if err == nil || !strings.Contains(err.Error(), "agentfw.stack-a.svc.cluster.local:8081") {
		t.Fatalf("svc: spec must fetch via service DNS in-cluster, got: %v", err)
	}
}

// Discovery registers every namespace's agentfw Service that exposes an
// admin port (named "admin" or numbered 8081) and skips everything else.
func TestRefreshAgentfwsDiscoveryFiltering(t *testing.T) {
	p := inClusterPortal(t,
		agentfwSvc("stack-a"),                                        // named admin port
		agentfwSvc("stack-b", corev1.ServicePort{Name: "proxy", Port: 8080},
			corev1.ServicePort{Name: "admin", Port: 8081}), // admin among others
		agentfwSvc("stack-c", corev1.ServicePort{Name: "web", Port: 8080}), // no admin port
		agentfwSvc("stack-d", corev1.ServicePort{Port: 8081}),              // unnamed but default port
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "stack-e"},
			Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "admin", Port: 8081}}}},
	)
	ctx := context.Background()
	p.refreshAgentfws(ctx)

	got := p.agentfwLabels()
	want := []string{"stack-a", "stack-b", "stack-d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("discovered = %v, want %v", got, want)
	}

	// A second refresh within the discovery interval is a no-op and must
	// not duplicate or drop items.
	p.refreshAgentfws(ctx)
	if again := p.agentfwLabels(); strings.Join(again, ",") != strings.Join(want, ",") {
		t.Fatalf("second refresh = %v, want %v", again, want)
	}
}

// Discovered (non-explicit) instances must never replace an explicit one.
func TestRefreshAgentfwsKeepsExplicit(t *testing.T) {
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_requests":42}`))
	}))
	defer es.Close()

	p := inClusterPortal(t, agentfwSvc("stack-a"))
	if err := p.SetAgentfwURL(es.URL + "#stack-a"); err != nil {
		t.Fatal(err)
	}
	p.refreshAgentfws(context.Background())

	items := p.agentfwSnapshot()
	if len(items) != 1 {
		t.Fatalf("items = %+v, want only the explicit instance", items)
	}
	raw, err := items[0].Fetch(context.Background(), "/api/v1/stats")
	if err != nil || !strings.Contains(string(raw), "42") {
		t.Fatalf("explicit fetch replaced by discovery? raw=%s err=%v", raw, err)
	}
}

// Aggregation: a product whose instance is unreachable is reported as
// offline with zeroed counters while healthy products still sum up.
func TestAgentfwStatsWithOfflineProduct(t *testing.T) {
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_requests":5,"blocked":1,"input_tokens":100,"output_tokens":50,` +
			`"cost_micro":9,"findings_by_kind":{"dlp":1},` +
			`"top_models":[{"model":"claude-sonnet-4-5","requests":5,"input_tokens":100,"out_tokens":50,"cost_micro":9}]}`))
	}))
	defer es.Close()

	p := New(nil)
	if err := p.SetAgentfwURL(es.URL + "#good"); err != nil {
		t.Fatal(err)
	}
	// A second, unreachable instance labeled "down".
	p.setAgentfwItem(agentfwItem{
		Label: "down",
		Fetch: func(ctx context.Context, path string) (json.RawMessage, error) {
			return nil, context.DeadlineExceeded
		},
	})

	mux := p.Mux()
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/api/v1/stats", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{`"total_requests":5`, `"blocked":1`, `"offline":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stats body missing %s: %s", want, body)
		}
	}

	r2 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/api/v1/products", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	b2 := w2.Body.String()
	if !strings.Contains(b2, `"product":"down"`) || !strings.Contains(b2, `"offline":true`) {
		t.Fatalf("products body must flag down product: %s", b2)
	}
	if !strings.Contains(b2, `"product":"good"`) || strings.Count(b2, `"offline"`) != 1 {
		t.Fatalf("good product must not be offline: %s", b2)
	}
}

// ?product=<ns> narrows fan-out to a single instance.
func TestAgentfwProductFilter(t *testing.T) {
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_requests":1}`))
	}))
	defer es.Close()

	p := New(nil)
	_ = p.SetAgentfwURL(es.URL + "#alpha")
	_ = p.SetAgentfwURL(es.URL + "#beta")

	mux := p.Mux()
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/agentfw/api/v1/stats?product=alpha", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if strings.Contains(w.Body.String(), "beta") {
		t.Fatalf("filtered query must not include beta: %s", w.Body.String())
	}
}
