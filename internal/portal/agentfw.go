package portal

// Multi-agentfw support: the portal discovers every tenant agentfw (a
// Service named "agentfw" with an admin port, one per Stack namespace),
// reaches each through its own managed `kubectl port-forward` child, and
// aggregates their archives into one API — every record labeled with the
// product (namespace) it belongs to.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"net/url"

	"github.com/einyx/kubo/internal/agentfw"
	corev1 "k8s.io/api/core/v1"
)

const (
	agentfwSvcName     = "agentfw"
	agentfwMount       = "/agentfw/api/v1"
	agentfwDiscEvery   = 30 * time.Second
	agentfwDefaultPort = 8081
)

// agentfwItem is one discovered or explicitly configured agentfw.
type agentfwItem struct {
	Label    string // product name = Stack namespace
	Explicit bool   // added via SetAgentfwURL; discovery never replaces it
	Fetch    func(ctx context.Context, path string) (json.RawMessage, error)
}

// agentfwResult is one instance's answer to a fan-out query.
type agentfwResult struct {
	label string
	raw   json.RawMessage
	err   error
}

// initAgentfwMaps lazily creates the registry maps (callers hold the lock).
func (p *Portal) initAgentfwMaps() {
	if p.agentfwItems == nil {
		p.agentfwItems = map[string]agentfwItem{}
	}
	if p.agentfwExplicit == nil {
		p.agentfwExplicit = map[string]bool{}
	}
}

// agentfwSnapshot returns the current items, sorted by label.
func (p *Portal) agentfwSnapshot() []agentfwItem {
	p.agentfwMu.RLock()
	defer p.agentfwMu.RUnlock()
	out := make([]agentfwItem, 0, len(p.agentfwItems))
	for _, lbl := range p.agentfwOrder {
		if it, ok := p.agentfwItems[lbl]; ok {
			out = append(out, it)
		}
	}
	return out
}

func (p *Portal) setAgentfwItem(it agentfwItem) {
	p.agentfwMu.Lock()
	defer p.agentfwMu.Unlock()
	p.initAgentfwMaps()
	if _, ok := p.agentfwItems[it.Label]; !ok {
		p.agentfwOrder = append(p.agentfwOrder, it.Label)
	}
	p.agentfwItems[it.Label] = it
	if it.Explicit {
		p.agentfwExplicit[it.Label] = true
	}
	sort.Strings(p.agentfwOrder)
}

// refreshAgentfws discovers agentfw Services across all namespaces, at most
// once per agentfwDiscEvery. Already-known products (explicit or discovered)
// are left alone.
func (p *Portal) refreshAgentfws(ctx context.Context) {
	p.agentfwMu.Lock()
	if p.client == nil || time.Since(p.agentfwDiscAt) < agentfwDiscEvery {
		p.agentfwMu.Unlock()
		return
	}
	p.agentfwDiscAt = time.Now()
	p.agentfwMu.Unlock()

	var svcs corev1.ServiceList
	if err := p.client.List(ctx, &svcs); err != nil {
		return // discovery is best-effort; explicit config still works
	}
	for _, svc := range svcs.Items {
		if svc.Name != agentfwSvcName {
			continue
		}
		lbl := svc.Namespace
		p.agentfwMu.RLock()
		_, known := p.agentfwItems[lbl]
		p.agentfwMu.RUnlock()
		if known {
			continue
		}
		for _, port := range svc.Spec.Ports {
			if port.Name != "admin" && int(port.Port) != agentfwDefaultPort {
				continue
			}
			fetch, err := p.agentfwPortForwardFetcher(svc.Namespace, int(port.Port))
			if err != nil {
				continue
			}
			p.setAgentfwItem(agentfwItem{Label: lbl, Fetch: fetch})
			break
		}
	}
}

// AgentfwEnabled reports whether any agentfw instance is configured.
func (p *Portal) AgentfwEnabled() bool {
	p.agentfwMu.RLock()
	defer p.agentfwMu.RUnlock()
	return len(p.agentfwItems) > 0
}

// agentfwLabels returns the known product labels, sorted.
func (p *Portal) agentfwLabels() []string {
	p.agentfwMu.RLock()
	defer p.agentfwMu.RUnlock()
	out := append([]string(nil), p.agentfwOrder...)
	sort.Strings(out)
	return out
}

// SetAgentfwURL registers an explicit agentfw instance. The label comes from
// the svc: namespace or the URL #fragment. Empty string removes all explicit
// instances; auto-discovery is independent and stays on.
func (p *Portal) SetAgentfwURL(raw string) error {
	raw = strings.TrimSpace(raw)
	p.agentfwMu.Lock()
	p.initAgentfwMaps()
	if raw == "" {
		for lbl := range p.agentfwExplicit {
			delete(p.agentfwItems, lbl)
			for i, l := range p.agentfwOrder {
				if l == lbl {
					p.agentfwOrder = append(p.agentfwOrder[:i], p.agentfwOrder[i+1:]...)
					break
				}
			}
		}
		p.agentfwExplicit = map[string]bool{}
		p.agentfwMu.Unlock()
		p.agentfwURL = ""
		return nil
	}
	p.agentfwMu.Unlock()
	p.agentfwURL = raw

	if strings.HasPrefix(raw, "svc:") {
		ns, svc, ok := strings.Cut(strings.TrimPrefix(raw, "svc:"), "/")
		if !ok || ns == "" || svc == "" {
			return fmt.Errorf("portal: invalid agentfw service spec %q (want svc:<namespace>/<service>[:<port>])", strings.TrimPrefix(raw, "svc:"))
		}
		fetch, err := p.agentfwPortForwardFetcher(ns, 0)
		if err != nil {
			return err
		}
		p.setAgentfwItem(agentfwItem{Label: ns, Explicit: true, Fetch: fetch})
		return nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("portal: invalid agentfw url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("portal: agentfw url must be http(s), got %q", raw)
	}
	label := u.Fragment
	base := &url.URL{Scheme: u.Scheme, Host: u.Host}
	fetch := func(ctx context.Context, path string) (json.RawMessage, error) {
		return fetchJSON(ctx, http.DefaultClient, base.String()+path)
	}
	p.setAgentfwItem(agentfwItem{Label: label, Explicit: true, Fetch: fetch})
	return nil
}

// agentfwPortForwardFetcher manages a `kubectl port-forward` child for the
// namespace's agentfw admin port and returns a fetcher over it. The child
// starts lazily on first fetch and restarts if it dies.
func (p *Portal) agentfwPortForwardFetcher(ns string, port int) (func(ctx context.Context, path string) (json.RawMessage, error), error) {
	if port == 0 {
		port = agentfwDefaultPort
	}
	local, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("portal: free local port for agentfw forward: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	var mu sync.Mutex
	up := false
	startLocked := func() { // caller must hold mu
		if up {
			return
		}
		cmd := exec.Command("kubectl", "-n", ns, "port-forward", "svc/"+agentfwSvcName,
			fmt.Sprintf("%d:%d", local, port))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("portal: agentfw port-forward failed to start (%s): %v", ns, err)
			return
		}
		up = true
		log.Printf("portal: agentfw port-forward started (%d -> %s/%s:%d)", local, ns, agentfwSvcName, port)
		go func() {
			_ = cmd.Wait()
			mu.Lock()
			up = false
			mu.Unlock()
			log.Printf("portal: agentfw port-forward exited (%s)", ns)
		}()
	}
	start := func() {
		mu.Lock()
		startLocked()
		mu.Unlock()
	}
	return func(ctx context.Context, path string) (json.RawMessage, error) {
		// Ensure the forward is up (spawn + wait, bounded — never while
		// holding the lock: startLocked needs it).
		deadline := time.Now().Add(4 * time.Second)
		for {
			mu.Lock()
			if up {
				mu.Unlock()
				break
			}
			startLocked()
			mu.Unlock()
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("agentfw port-forward could not start (%s)", ns)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		raw, err := fetchJSON(ctx, client, fmt.Sprintf("http://127.0.0.1:%d%s", local, path))
		if err != nil {
			mu.Lock()
			was := up
			mu.Unlock()
			if was {
				go start() // forward died mid-request — restart for the next one
			}
			return nil, err
		}
		return raw, nil
	}, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func fetchJSON(ctx context.Context, client *http.Client, url string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream %s: %d: %s", url, resp.StatusCode, truncate(string(body), 160))
	}
	return json.RawMessage(body), nil
}

// handleAgentfwAPI serves the aggregated agentfw archive API. Every record
// carries a "product" field naming the Stack namespace it came from; the
// query is forwarded to each instance, and ?product=<ns> narrows to one.
func (p *Portal) handleAgentfwAPI(w http.ResponseWriter, r *http.Request) {
	p.refreshAgentfws(r.Context())
	items := p.agentfwSnapshot()
	if len(items) == 0 {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, agentfwMount)
	if suffix == "" {
		suffix = "/"
	}
	apiPath := "/api/v1" + suffix // instances serve under /api/v1

	product := r.URL.Query().Get("product")
	if product != "" {
		filtered := make([]agentfwItem, 0, 1)
		for _, it := range items {
			if it.Label == product {
				filtered = append(filtered, it)
			}
		}
		items = filtered
	}

	q := r.URL.Query()
	q.Del("product")
	forwarded := ""
	if enc := q.Encode(); enc != "" {
		forwarded = "?" + enc
	}

	results := make([]agentfwResult, len(items))
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func(i int, it agentfwItem) {
			defer wg.Done()
			raw, err := it.Fetch(r.Context(), apiPath+forwarded)
			results[i] = agentfwResult{label: it.Label, raw: raw, err: err}
		}(i, it)
	}
	wg.Wait()

	switch {
	case suffix == "/" || suffix == "/healthz":
		health := map[string]any{}
		okCount := 0
		for _, res := range results {
			health[res.label] = map[string]any{"ok": res.err == nil, "error": errString(res.err)}
			if res.err == nil {
				okCount++
			}
		}
		status := "down"
		if okCount == len(results) {
			status = "ok"
		} else if okCount > 0 {
			status = "partial"
		}
		writeAgentfwJSON(w, http.StatusOK, map[string]any{"status": status, "products": health})

	case suffix == "/products":
		// Instances have no /products route — ask each for /stats instead.
		out := []map[string]any{}
		for _, it := range items {
			pp := map[string]any{"product": it.Label}
			raw, err := it.Fetch(r.Context(), "/api/v1/stats")
			if err == nil {
				var s agentfw.Stats
				if json.Unmarshal(raw, &s) == nil {
					pp["total_requests"] = s.TotalRequests
					pp["blocked"] = s.Blocked
					pp["input_tokens"] = s.InputTokens
					pp["output_tokens"] = s.OutputTokens
					pp["cost_micro"] = s.CostMicro
				}
			} else {
				pp["offline"] = true
			}
			out = append(out, pp)
		}
		writeAgentfwJSON(w, http.StatusOK, map[string]any{"products": out})

	case suffix == "/stats":
		out := agentfw.Stats{FindingsByKind: map[string]int{}, TopModels: []agentfw.ModelUsage{}}
		products := []map[string]any{}
		blocked := []map[string]any{}
		modelIdx := map[string]int{}
		for _, res := range results {
			pp := map[string]any{"product": res.label, "total_requests": 0, "blocked": 0,
				"redacted": 0, "input_tokens": int64(0), "output_tokens": int64(0), "cost_micro": int64(0)}
			if res.err != nil {
				pp["offline"] = true
				products = append(products, pp)
				continue
			}
			var s agentfw.Stats
			if json.Unmarshal(res.raw, &s) != nil {
				continue
			}
			out.TotalRequests += s.TotalRequests
			out.TotalSessions += s.TotalSessions
			out.Blocked += s.Blocked
			out.Redacted += s.Redacted
			out.InputTokens += s.InputTokens
			out.OutputTokens += s.OutputTokens
			out.CostMicro += s.CostMicro
			for k, n := range s.FindingsByKind {
				out.FindingsByKind[k] += n
			}
			pp["total_requests"] = s.TotalRequests
			pp["blocked"] = s.Blocked
			pp["redacted"] = s.Redacted
			pp["input_tokens"] = s.InputTokens
			pp["output_tokens"] = s.OutputTokens
			pp["cost_micro"] = s.CostMicro
			products = append(products, pp)
			for _, m := range s.TopModels {
				if i, ok := modelIdx[m.Model]; ok {
					out.TopModels[i].Requests += m.Requests
					out.TopModels[i].InputTokens += m.InputTokens
					out.TopModels[i].OutTokens += m.OutTokens
					out.TopModels[i].CostMicro += m.CostMicro
				} else {
					modelIdx[m.Model] = len(out.TopModels)
					out.TopModels = append(out.TopModels, m)
				}
			}
			for _, rec := range s.RecentBlocked {
				blocked = append(blocked, withProduct(rec, res.label))
			}
		}
		sort.Slice(blocked, func(i, j int) bool {
			return afwTimeOf(blocked[i]["time"]).After(afwTimeOf(blocked[j]["time"]))
		})
		if len(blocked) > 8 {
			blocked = blocked[:8]
		}
		sort.Slice(out.TopModels, func(i, j int) bool { return out.TopModels[i].CostMicro > out.TopModels[j].CostMicro })
		writeAgentfwJSON(w, http.StatusOK, map[string]any{
			"total_requests": out.TotalRequests, "total_sessions": out.TotalSessions,
			"blocked": out.Blocked, "redacted": out.Redacted,
			"findings_by_kind": out.FindingsByKind, "top_models": out.TopModels,
			"input_tokens": out.InputTokens, "output_tokens": out.OutputTokens,
			"cost_micro": out.CostMicro, "recent_blocked": blocked, "products": products,
		})

	case suffix == "/sessions":
		p.mergeAgentfwLists(w, r, results, "sessions")

	case suffix == "/requests":
		p.mergeAgentfwLists(w, r, results, "requests")

	case strings.HasPrefix(suffix, "/requests/"):
		id := strings.Trim(strings.TrimPrefix(suffix, "/requests"), "/")
		for _, it := range items {
			raw, err := it.Fetch(r.Context(), "/requests/"+id)
			if err == nil {
				writeAgentfwJSON(w, http.StatusOK, withProductRaw(raw, it.Label))
				return
			}
		}
		http.NotFound(w, r)

	case suffix == "/usage":
		var input, output, cost int64
		models := []map[string]any{}
		idx := map[string]int{}
		for _, res := range results {
			if res.err != nil {
				continue
			}
			var u struct {
				Input  int64                `json:"input_tokens"`
				Output int64                `json:"output_tokens"`
				Cost   int64                `json:"cost_micro"`
				Models []agentfw.ModelUsage `json:"models"`
			}
			if json.Unmarshal(res.raw, &u) != nil {
				continue
			}
			input += u.Input
			output += u.Output
			cost += u.Cost
			for _, m := range u.Models {
				if i, ok := idx[m.Model]; ok {
					models[i]["requests"] = models[i]["requests"].(int) + m.Requests
					models[i]["input_tokens"] = models[i]["input_tokens"].(int64) + m.InputTokens
					models[i]["output_tokens"] = models[i]["output_tokens"].(int64) + m.OutTokens
					models[i]["cost_micro"] = models[i]["cost_micro"].(int64) + m.CostMicro
				} else {
					idx[m.Model] = len(models)
					models = append(models, map[string]any{"model": m.Model, "requests": m.Requests,
						"input_tokens": m.InputTokens, "output_tokens": m.OutTokens, "cost_micro": m.CostMicro})
				}
			}
		}
		sort.Slice(models, func(i, j int) bool {
			return models[i]["cost_micro"].(int64) > models[j]["cost_micro"].(int64)
		})
		writeAgentfwJSON(w, http.StatusOK, map[string]any{
			"input_tokens": input, "output_tokens": output, "cost_micro": cost, "models": models})

	default:
		http.NotFound(w, r)
	}
}

// mergeAgentfwLists combines per-instance record lists (requests or
// sessions), annotates each record with "product", sorts newest-first, and
// applies the offset/limit window over the merged set.
func (p *Portal) mergeAgentfwLists(w http.ResponseWriter, r *http.Request, results []agentfwResult, key string) {
	q := r.URL.Query()
	limit := atoiDefault(q.Get("limit"), 100)
	offset := atoiDefault(q.Get("offset"), 0)
	listKey, timeKey := "requests", "time"
	if key == "sessions" {
		listKey, timeKey = "sessions", "last_seen"
	}
	merged := []map[string]any{}
	total := 0
	for _, res := range results {
		if res.err != nil {
			continue
		}
		// Instances answer {"requests":[…],"total":N} and {"sessions":[…]}
		// (no total) — decode the endpoint's own envelope key.
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(res.raw, &envelope); err != nil {
			continue
		}
		var rows []map[string]any
		if raw, ok := envelope[listKey]; ok {
			_ = json.Unmarshal(raw, &rows)
		}
		for _, row := range rows {
			row["product"] = res.label
			merged = append(merged, row)
		}
		var t int
		if raw, ok := envelope["total"]; ok {
			_ = json.Unmarshal(raw, &t)
		} else {
			t = len(rows)
		}
		total += t
	}
	sort.Slice(merged, func(i, j int) bool {
		return afwTimeOf(merged[i][timeKey]).After(afwTimeOf(merged[j][timeKey]))
	})
	if offset > len(merged) {
		offset = len(merged)
	}
	end := offset + limit
	if end > len(merged) {
		end = len(merged)
	}
	page := merged[offset:end]
	if page == nil {
		page = []map[string]any{}
	}
	writeAgentfwJSON(w, http.StatusOK, map[string]any{key: page, "total": total})
}

// afwTimeOf parses the RFC3339 timestamps used by the archive API.
func afwTimeOf(v any) time.Time {
	s, _ := v.(string)
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// withProduct attaches the product label to a record.
func withProduct(rec agentfw.Record, label string) map[string]any {
	b, _ := json.Marshal(rec)
	m := map[string]any{}
	if json.Unmarshal(b, &m) != nil {
		return map[string]any{"product": label}
	}
	m["product"] = label
	return m
}

func mustRaw(m map[string]any) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}

// withProductRaw decodes a raw record and attaches its product label.
func withProductRaw(raw json.RawMessage, label string) map[string]any {
	m := map[string]any{}
	if json.Unmarshal(raw, &m) != nil {
		return map[string]any{"product": label}
	}
	m["product"] = label
	return m
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeAgentfwJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
