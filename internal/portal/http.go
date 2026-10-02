package portal

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Mux returns the portal HTTP handler:
//
//	GET  /                       single-page UI
//	GET  /api/stacks             JSON list of all stacks
//	GET  /api/stacks/{ns}/{name} component-level detail
//	GET  /api/template?tenant=x  rendered template YAML (dry run)
//	POST /api/stacks             {"tenant":"x","mode":"Direct"} → create
func (p *Portal) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(p.GetIndexHTML()))
	})
	mux.HandleFunc("GET /api/stacks", func(w http.ResponseWriter, r *http.Request) {
		stacks, err := p.ListStacks(r.Context())
		respond(w, r, stacks, err)
	})
	mux.HandleFunc("GET /api/template", func(w http.ResponseWriter, r *http.Request) {
		out, err := p.CreateFromTemplate(r.Context(), r.URL.Query().Get("tenant"), "Direct", true)
		respondYAML(w, r, out, err)
	})
	mux.HandleFunc("POST /api/stacks", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tenant string `json:"tenant"`
			Mode   string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respond(w, r, nil, fmt.Errorf("invalid JSON body"))
			return
		}
		if req.Mode == "" {
			req.Mode = "Direct"
		}
		if _, err := p.CreateFromTemplate(r.Context(), req.Tenant, req.Mode, false); err != nil {
			respond(w, r, nil, err)
			return
		}
		respond(w, r, map[string]bool{"created": true}, nil)
	})
	mux.HandleFunc("GET /api/stacks/{namespace}/{name}", func(w http.ResponseWriter, r *http.Request) {
		d, err := p.GetStack(r.Context(), r.PathValue("namespace"), r.PathValue("name"))
		respond(w, r, d, err)
	})
	return mux
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
