package portal

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Mux returns the portal HTTP handler:
//
//	GET  /                        single-page UI
//	GET  /api/stacks              JSON list of all stacks
//	GET  /api/stacks/{ns}/{name}  component-level detail
//	GET  /api/stacks/{ns}/{name}/yaml  live manifest
//	POST /api/stacks/{ns}/{name}/reconcile  trigger a controller reconcile
//	PATCH /api/stacks/{ns}/{name}  partial spec update (mode/bundle/exclude/operators)
//	GET  /api/stacks/{ns}/events  recent namespace events
//	POST /api/stacks/{ns}/{name}/pause  {"paused":true|false}
//	GET  /api/backups          all StackBackups (newest first)
//	POST /api/backups          {"sourceNamespace","targetNamespace","include"} → create
//	DELETE /api/stacks/{ns}/{name}?confirm=<ns>&purge=true
//	GET  /api/templates           template registry (metadata only)
//	GET  /api/template            rendered YAML preview (template + tenant)
//	POST /api/stacks              {"template","tenant","mode",...} → create
//
// The portal is unauthenticated by design (it is a local operator tool):
// requests whose Host is not a loopback form are rejected, which defeats
// DNS-rebinding drive-by attacks against a developer's kubeconfig.
func (p *Portal) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Write([]byte(p.GetIndexHTML()))
	})
	mux.HandleFunc("GET /api/stacks", func(w http.ResponseWriter, r *http.Request) {
		stacks, err := p.ListStacks(r.Context())
		respond(w, r, stacks, err)
	})
	mux.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		templates, err := p.registry.List(r.Context())
		respond(w, r, templates, err)
	})
	mux.HandleFunc("GET /api/template", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := CreateRequest{
			Template: q.Get("template"),
			Tenant:   q.Get("tenant"),
			Mode:     q.Get("mode"),
		}
		out, err := p.CreateFromTemplate(r.Context(), req, true)
		respondYAML(w, r, out, err)
	})
	mux.HandleFunc("POST /api/stacks", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		d, err := p.GetStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		respond(w, r, d, err)
	})
	mux.HandleFunc("GET /api/stacks/{namespace}/{name}/yaml", func(w http.ResponseWriter, r *http.Request) {
		out, err := p.GetStackYAML(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		respondYAML(w, r, out, err)
	})
	mux.HandleFunc("POST /api/stacks/{namespace}/{name}/reconcile", func(w http.ResponseWriter, r *http.Request) {
		err := p.ReconcileStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"reconciled": true}, nil)
	})
	mux.HandleFunc("PATCH /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /api/stacks/{namespace}/events", func(w http.ResponseWriter, r *http.Request) {
		events, err := p.ListStackEvents(r.Context(), r.PathValue("namespace"))
		respond(w, r, events, err)
	})
	mux.HandleFunc("POST /api/stacks/{namespace}/{name}/pause", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("GET /api/backups", func(w http.ResponseWriter, r *http.Request) {
		backups, err := p.ListStackBackups(r.Context())
		respond(w, r, backups, err)
	})
	mux.HandleFunc("POST /api/backups", func(w http.ResponseWriter, r *http.Request) {
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
	mux.HandleFunc("DELETE /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		err := p.DeleteStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"),
			q.Get("confirm"), q.Get("purge") == "true")
		if err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"deleted": true}, nil)
	})
	return loopbackHostOnly(mux)
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

func respond(w http.ResponseWriter, r *http.Request, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(v)
}

func respondYAML(w http.ResponseWriter, r *http.Request, yamlText string, err error) {
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml")
	w.Write([]byte(yamlText))
}
