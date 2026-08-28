package portal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Mux returns the portal HTTP handler:
//
//	GET  /                        single-page UI
//	GET  /api/stacks              JSON list of all stacks (ETag-cached)
//	GET  /api/stacks/{ns}/{name}  component-level detail
//	GET  /api/stacks/{ns}/{name}/yaml  live manifest
//	POST /api/stacks/{ns}/{name}/reconcile  trigger a controller reconcile
//	POST /api/stacks/{ns}/{name}/pause  {"paused":true|false}
//	PATCH /api/stacks/{ns}/{name}  partial spec update (mode/bundle/exclude/operators/values)
//	GET  /api/stacks/{ns}/events  recent namespace events (?limit=, default 50)
//	GET  /api/backups          all StackBackups (newest first)
//	POST /api/backups          {"sourceNamespace","targetNamespace","include"} → create
//	POST /api/backups/{ns}/{name}/retry  create a fresh copy of a finished backup
//	DELETE /api/backups/{ns}/{name}  delete a backup record (Job is GC'd via ownerRef)
//	DELETE /api/stacks/{ns}/{name}?confirm=<ns>&purge=true
//	GET  /api/templates           template registry (metadata only)
//	GET  /api/template            rendered YAML preview (template + tenant)
//	POST /api/stacks              {"template","tenant","mode",...} → create
//	GET  /healthz              liveness (always OK once serving)
//	GET  /readyz               readiness (K8s API reachable)
//	GET  /metrics              Prometheus metrics
//
// The portal is unauthenticated by design (it is a local operator tool):
// requests whose Host is not a loopback form are rejected, which defeats
// DNS-rebinding drive-by attacks against a developer's kubeconfig.
// Mutating requests are additionally rate limited per client IP.
func (p *Portal) Mux() http.Handler {
	m := p.metrics
	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		if m != nil {
			mux.Handle(pattern, m.instrument(pattern, h))
			return
		}
		mux.HandleFunc(pattern, h)
	}

	handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write([]byte(p.GetIndexHTML()))
	})
	handle("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	handle("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := contextWithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if _, err := p.ListStacks(ctx); err != nil {
			http.Error(w, "kubernetes api unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	if m != nil {
		mux.Handle("GET /metrics", m.Handler())
	}
	handle("GET /api/stacks", func(w http.ResponseWriter, r *http.Request) {
		stacks, err := p.ListStacks(r.Context())
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		// ETag on the serialized list lets the UI poll cheaply: an unchanged
		// cluster yields a 304 with no body.
		body, _ := json.Marshal(stacks)
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})
	handle("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		templates, err := p.registry.List(r.Context())
		respond(w, r, templates, err)
	})
	handle("GET /api/template", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := CreateRequest{
			Template: q.Get("template"),
			Tenant:   q.Get("tenant"),
			Mode:     q.Get("mode"),
		}
		out, err := p.CreateFromTemplate(r.Context(), req, true)
		respondYAML(w, r, out, err)
	})
	handle("POST /api/stacks", func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		if _, err := p.CreateFromTemplate(r.Context(), req, false); err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"created": true}, nil)
	})
	handle("GET /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		d, err := p.GetStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		respond(w, r, d, err)
	})
	handle("GET /api/stacks/{namespace}/{name}/yaml", func(w http.ResponseWriter, r *http.Request) {
		out, err := p.GetStackYAML(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		respondYAML(w, r, out, err)
	})
	handle("POST /api/stacks/{namespace}/{name}/reconcile", func(w http.ResponseWriter, r *http.Request) {
		err := p.ReconcileStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"reconciled": true}, nil)
	})
	handle("POST /api/stacks/{namespace}/{name}/pause", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paused bool `json:"paused"`
		}
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		if err := p.SetStackPaused(r.Context(), r.PathValue("namespace"), r.PathValue("name"), req.Paused); err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"paused": req.Paused}, nil)
	})
	handle("PATCH /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req PatchRequest
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		if err := p.PatchStackSpec(r.Context(), r.PathValue("namespace"), r.PathValue("name"), req); err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"patched": true}, nil)
	})
	handle("GET /api/stacks/{namespace}/events", func(w http.ResponseWriter, r *http.Request) {
		events, err := p.ListStackEvents(r.Context(), r.PathValue("namespace"), atoiDefault(r.URL.Query().Get("limit"), 50))
		respond(w, r, events, err)
	})
	handle("GET /api/backups", func(w http.ResponseWriter, r *http.Request) {
		backups, err := p.ListStackBackups(r.Context())
		respond(w, r, backups, err)
	})
	handle("POST /api/backups", func(w http.ResponseWriter, r *http.Request) {
		var req BackupRequest
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		bk, err := p.CreateStackBackup(r.Context(), req)
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, bk, nil)
	})
	handle("POST /api/backups/{namespace}/{name}/retry", func(w http.ResponseWriter, r *http.Request) {
		bk, err := p.RetryStackBackup(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, bk, nil)
	})
	handle("DELETE /api/backups/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.DeleteStackBackup(r.Context(), r.PathValue("namespace"), r.PathValue("name")); err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"deleted": true}, nil)
	})
	handle("DELETE /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		err := p.DeleteStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"),
			q.Get("confirm"), q.Get("purge") == "true")
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"deleted": true}, nil)
	})
	return rateLimitMutations(loopbackHostOnly(mux))
}

// rateLimitMutations caps mutating requests (POST/PATCH/DELETE) per client
// IP in a token-bucket (burst 20, one token every 3 seconds) to blunt
// foot-guns and accidental request storms. Loopback tools rarely need more.
type bucket struct {
	tokens float64
	last   time.Time
}

var (
	bucketsMu sync.Mutex
	buckets   = map[string]*bucket{}
)

const (
	rateBurst    = 20
	rateInterval = 3 * time.Second
)

// resetRateLimits clears per-IP buckets; used by tests to isolate state.
func resetRateLimits() {
	bucketsMu.Lock()
	buckets = map[string]*bucket{}
	bucketsMu.Unlock()
}

func rateLimitMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodDelete:
		default:
			next.ServeHTTP(w, r)
			return
		}
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		now := time.Now()
		bucketsMu.Lock()
		b, ok := buckets[ip]
		if !ok {
			if len(buckets) > 1024 { // shed state, not correctness
				buckets = map[string]*bucket{}
			}
			b = &bucket{tokens: rateBurst, last: now}
			buckets[ip] = b
		}
		b.tokens = min(rateBurst, b.tokens+now.Sub(b.last).Seconds()/rateInterval.Seconds())
		b.last = now
		allowed := b.tokens >= 1
		if allowed {
			b.tokens--
		}
		bucketsMu.Unlock()
		if !allowed {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"too many requests — slow down"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusForError maps Kubernetes API errors to sensible HTTP codes; the
// remaining portal errors are caller-input problems (400) by construction.
func statusForError(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case apierrors.IsNotFound(err):
		return http.StatusNotFound
	case apierrors.IsAlreadyExists(err):
		return http.StatusConflict
	case apierrors.IsConflict(err):
		return http.StatusConflict
	case apierrors.IsForbidden(err):
		return http.StatusForbidden
	default:
		return http.StatusBadRequest
	}
}

func respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(statusForError(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(v)
}

// loopbackHostOnly rejects requests whose Host header is not a loopback
// form. DNS-rebinding attacks make a browser send an attacker-chosen Host
// while connecting to 127.0.0.1; blocking non-loopback Hosts closes that
// route without breaking normal local use.
func loopbackHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host != "localhost" {
			if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"portal: loopback access only"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func respondYAML(w http.ResponseWriter, r *http.Request, yamlText string, err error) {
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	w.Write([]byte(yamlText))
}
