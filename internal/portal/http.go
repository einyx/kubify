package portal

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/einyx/kubo/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
//	GET  /api/marketplace/health  Marketplace credential readiness
//	POST /api/marketplace/resolve  resolve a Marketplace purchase token
//	POST /api/marketplace/activate activate a resolved subscription
//
// The portal is unauthenticated by design (it is a local operator tool):
// requests whose Host is not a loopback form are rejected, which defeats
// DNS-rebinding drive-by attacks against a developer's kubeconfig.
// Mutating requests are additionally rate limited per client IP.
func (p *Portal) Mux() http.Handler {
	return p.mux(false)
}

// RemoteMux serves the portal behind an in-cluster Service or authenticating proxy.
func (p *Portal) RemoteMux() http.Handler {
	return p.mux(true)
}

func (p *Portal) mux(allowRemote bool) http.Handler {
	m := p.metrics
	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		if m != nil {
			mux.Handle(pattern, m.instrument(pattern, h))
			return
		}
		mux.HandleFunc(pattern, h)
	}

	handle("GET /assets/", serveAsset)
	handle("GET /marketplace", serveMarketplaceLanding)
	handle("POST /api/marketplace/webhook", p.handleMarketplaceWebhook)
	handle("GET /api/marketplace/health", func(w http.ResponseWriter, r *http.Request) {
		_, err := p.marketplaceClient(r.Context())
		respond(w, r, map[string]bool{"configured": err == nil}, err)
	})
	handle("POST /api/marketplace/resolve", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		out, err := p.ResolveMarketplace(r.Context(), in.Token)
		respond(w, r, out, err)
	})
	handle("POST /api/marketplace/activate", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SubscriptionID string `json:"subscriptionId"`
			PlanID         string `json:"planId"`
			Quantity       int32  `json:"quantity"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		request, err := p.ActivateMarketplace(r.Context(), in.SubscriptionID, in.PlanID, in.Quantity)
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]string{"name": request.Name, "phase": string(request.Status.Phase)}, nil)
	})
	handle("GET /api/marketplace/requests/{name}", func(w http.ResponseWriter, r *http.Request) {
		var request v1alpha1.MarketplaceRequest
		err := p.client.Get(r.Context(), client.ObjectKey{Namespace: "kubo-system", Name: r.PathValue("name")}, &request)
		respond(w, r, map[string]string{
			"phase": string(request.Status.Phase), "message": request.Status.Message,
			"tenant": request.Status.Tenant, "url": request.Status.URL,
		}, err)
	})
	handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		p.refreshAgentfws(r.Context()) // nav reflects discovered products immediately
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		html := strings.Replace(p.GetIndexHTML(), "<!-- mcp-nav -->", p.mcpNav(), 1)
		html = strings.Replace(html, "<!-- agentfw-nav -->", p.agentfwNav(), 1)
		w.Write([]byte(html))
	})
	// agentfw archive API: all discovered agentfw instances (plus any
	// explicit -agentfw config) aggregated under one mount; every record
	// labeled with its product (namespace). Routes stay registered even
	// before discovery fills in.
	handle("GET /agentfw", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/#/agents", http.StatusPermanentRedirect)
	})
	handle("GET /agentfw/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/#/agents", http.StatusPermanentRedirect)
	})
	handle("GET /agentfw/api/v1/", p.handleAgentfwAPI)
	handle("GET /agentfw/api/v1/{$}", p.handleAgentfwAPI)
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
	// Demo tenant lifecycle: list/approve/reject/extend DemoRequests.
	handle("GET /api/demorequests", func(w http.ResponseWriter, r *http.Request) {
		out, err := p.ListDemoRequests(r.Context())
		respond(w, r, out, err)
	})
	handle("POST /api/demorequests/{name}/approve", func(w http.ResponseWriter, r *http.Request) {
		respond(w, r, map[string]bool{"ok": true}, p.ApproveDemoRequest(r.Context(), r.PathValue("name")))
	})
	handle("PATCH /api/demorequests/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in EditDemoRequestIn
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body: {\"email\": ..., \"company\": ...}"))
			return
		}
		respond(w, r, map[string]bool{"ok": true}, p.UpdateDemoRequest(r.Context(), r.PathValue("name"), in))
	})
	handle("DELETE /api/demorequests/{name}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, r, map[string]bool{"ok": true}, p.DeleteDemoRequest(r.Context(), r.PathValue("name")))
	})
	handle("POST /api/demorequests/{name}/reject", func(w http.ResponseWriter, r *http.Request) {
		respond(w, r, map[string]bool{"ok": true}, p.RejectDemoRequest(r.Context(), r.PathValue("name")))
	})
	handle("POST /api/demorequests/{name}/extend", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Hours int `json:"hours"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Hours == 0 {
			respond(w, r, nil, fmt.Errorf("invalid JSON body: {\"hours\": 24}"))
			return
		}
		respond(w, r, map[string]bool{"ok": true}, p.ExtendDemoRequest(r.Context(), r.PathValue("name"), req.Hours))
	})
	handle("GET /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		d, err := p.GetStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		respond(w, r, d, err)
	})
	// Website demo requests: bearer-token-authed (the website's own Turnstile
	// + rate limiting runs first at the edge). Disabled unless the
	// kubo-system/demo-requests-token secret exists.
	handle("POST /api/demorequests", func(w http.ResponseWriter, r *http.Request) {
		token := p.demoRequestToken(r.Context())
		if token == "" {
			http.Error(w, "demo requests disabled", http.StatusServiceUnavailable)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || !constantTimeEq(got, token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var in DemoRequestInbound
		body := http.MaxBytesReader(w, r.Body, 16<<10)
		if err := json.NewDecoder(body).Decode(&in); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		dr, err := p.CreateDemoRequest(r.Context(), in)
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]string{
			"name":   dr.Name,
			"phase":  string(dr.Status.Phase),
			"tenant": dr.Status.Tenant,
		}, nil)
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
	handle("POST /api/stacks/{namespace}/{name}/feature-flags/writeback", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			FeatureFlags map[string]string `json:"featureFlags"`
			Expected     map[string]string `json:"expectedFeatureFlags"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		who := r.Header.Get("Cf-Access-Authenticated-User-Email")
		if who == "" {
			who = "portal"
		}
		out, err := p.CreateFeatureFlagWriteback(r.Context(), r.PathValue("namespace"), r.PathValue("name"), who, in.FeatureFlags, in.Expected)
		respond(w, r, out, err)
	})
	for path, field := range map[string]string{"image-tags": "imageTags", "chart-versions": "chartVersions"} {
		field := field
		handle("POST /api/stacks/{namespace}/{name}/"+path+"/writeback", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Desired  map[string]string `json:"desired"`
				Expected map[string]string `json:"expected"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
				respond(w, r, nil, fmt.Errorf("invalid JSON body"))
				return
			}
			who := r.Header.Get("Cf-Access-Authenticated-User-Email")
			if who == "" {
				who = "portal"
			}
			out, err := p.CreateStackMapWriteback(r.Context(), r.PathValue("namespace"), r.PathValue("name"), who, field, in.Desired, in.Expected)
			respond(w, r, out, err)
		})
	}
	handle("GET /api/stacks/{namespace}/{name}/components/{component}/pods", func(w http.ResponseWriter, r *http.Request) {
		pods, err := p.ListComponentPods(r.Context(), r.PathValue("namespace"), r.PathValue("component"))
		respond(w, r, pods, err)
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

	// --- Vault (per-tenant bank-vaults KV) ---
	// Values are redacted unless the request explicitly passes reveal=true.
	// The Vault root token never appears in any response.
	handle("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		p.stackEvents(w, r)
	})
	handle("GET /api/mcp", func(w http.ResponseWriter, r *http.Request) {
		info := p.MCPInfo(r.Context())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})
	handle("GET /api/mcp/tools", func(w http.ResponseWriter, r *http.Request) {
		if p.mcpCaller == nil {
			respond(w, r, nil, fmt.Errorf("MCP is not wired into this portal"))
			return
		}
		schemas, err := p.mcpCaller.ToolSchemas(r.Context())
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Write(schemas)
	})
	handle("POST /api/mcp/call", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		out, err := p.MCPCall(r.Context(), req.Name, req.Arguments)
		respond(w, r, out, err)
	})
	handle("GET /api/namespaces/{namespace}/vault/health", func(w http.ResponseWriter, r *http.Request) {
		h, err := p.VaultHealth(r.Context(), r.PathValue("namespace"))
		respond(w, r, h, err)
	})
	handle("GET /api/namespaces/{namespace}/vault/tree", func(w http.ResponseWriter, r *http.Request) {
		entries, folders, err := p.VaultList(r.Context(), r.PathValue("namespace"), strings.Trim(r.URL.Query().Get("path"), "/"))
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]interface{}{"entries": entries, "folders": folders}, nil)
	})
	handle("GET /api/namespaces/{namespace}/vault/entry", func(w http.ResponseWriter, r *http.Request) {
		reveal := r.URL.Query().Get("reveal") == "true"
		e, err := p.VaultRead(r.Context(), r.PathValue("namespace"), strings.Trim(r.URL.Query().Get("path"), "/"), reveal)
		respond(w, r, e, err)
	})
	handle("PUT /api/namespaces/{namespace}/vault/entry", func(w http.ResponseWriter, r *http.Request) {
		body := http.MaxBytesReader(w, r.Body, 1<<20)
		var req VaultWriteRequest
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		if err := p.VaultWrite(r.Context(), r.PathValue("namespace"), req); err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"written": true}, nil)
	})
	handle("DELETE /api/namespaces/{namespace}/vault/entry", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if permanent := q.Get("permanent") == "true"; permanent && q.Get("confirm") != r.PathValue("namespace") {
			respond(w, r, nil, fmt.Errorf("permanent delete requires confirm=<namespace>"))
			return
		}
		if err := p.VaultDelete(r.Context(), r.PathValue("namespace"), strings.Trim(q.Get("path"), "/"), q.Get("permanent") == "true"); err != nil {
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
	var handler http.Handler = mux
	if !allowRemote {
		handler = loopbackHostOnly(handler)
	}
	return rateLimitMutations(sameOriginMutations(handler))
}

// sameOriginMutations blocks cross-site browser-initiated mutations (CSRF).
// The loopback Host check defeats DNS rebinding but not a malicious page
// form-posting to http://localhost:9090 from the user's browser. State
// -changing requests are therefore accepted only when:
//
//   - an Origin header is present and its host matches the request Host
//     (same-origin), or
//   - Sec-Fetch-Site says same-origin / none, or
//   - neither header is present (curl, scripts — non-browser clients).
//
// Read-only GETs are exempt: the portal sets no cookies or ambient
// credentials, so cross-site GETs leak nothing and CORS blocks the reads.
func sameOriginMutations(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPatch, http.MethodDelete:
		default:
			next.ServeHTTP(w, r)
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil {
				rejectCrossSite(w, "malformed Origin")
				return
			}
			ohost := u.Hostname()
			if !sameHost(ohost, host) {
				rejectCrossSite(w, "cross-site Origin")
				return
			}
		} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" &&
			site != "same-origin" && site != "none" {
			rejectCrossSite(w, "cross-site request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameHost compares hosts, tolerating an explicit loopback alias
// (127.0.0.1 vs ::1 vs localhost all belong to this portal).
func sameHost(a, b string) bool {
	if a == b {
		return true
	}
	ipA, ipB := net.ParseIP(a), net.ParseIP(b)
	if ipA != nil && ipB != nil && ipA.IsLoopback() && ipB.IsLoopback() {
		return true
	}
	loopNames := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	return loopNames[a] && loopNames[b]
}

func rejectCrossSite(w http.ResponseWriter, why string) {
	w.WriteHeader(http.StatusForbidden)
	w.Write([]byte(`{"error":"portal: ` + why + ` — mutating requests must be same-origin"}`))
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

// constantTimeEq compares two strings without early exit.
func constantTimeEq(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
