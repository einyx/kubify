package agentfw

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
)

// Viewer serves the agentsview-style archive UI and API on the admin port.
type Viewer struct {
	archive atomic.Pointer[Archive] // nil until wired; handlers degrade to 503
	mux     *http.ServeMux
}

// NewViewer builds the viewer route table.
func NewViewer() *Viewer {
	v := &Viewer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/stats", v.handleStats)
	mux.HandleFunc("GET /api/v1/sessions", v.handleSessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}", v.handleSessionDetail)
	mux.HandleFunc("GET /api/v1/requests", v.handleRequests)
	mux.HandleFunc("GET /api/v1/requests/{id}", v.handleRequest)
	mux.HandleFunc("GET /api/v1/usage", v.handleUsage)
	mux.HandleFunc("GET /api/v1/healthz", v.handleHealth)
	mux.HandleFunc("GET /", v.handleUI)
	v.mux = mux
	return v
}

// SetArchive wires (or replaces) the backing archive.
func (v *Viewer) SetArchive(a *Archive) { v.archive.Store(a) }

// Handler returns the viewer's HTTP handler (UI + API).
func (v *Viewer) Handler() http.Handler { return v.mux }

var pathIDRe = regexp.MustCompile(`[^a-zA-Z0-9:_\-.]`)

func sanitizeID(s string) string { return pathIDRe.ReplaceAllString(s, "") }

func writeJSON(w http.ResponseWriter, code int, val any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(val)
}

func (v *Viewer) archiveOr503(w http.ResponseWriter) *Archive {
	a := v.archive.Load()
	if a == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "archive not available (view disabled or failed to open)"})
		return nil
	}
	return a
}

func (v *Viewer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if v.archive.Load() == nil {
		writeJSON(w, http.StatusOK, map[string]any{"status": "degraded", "archive": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "archive": true})
}

func (v *Viewer) handleStats(w http.ResponseWriter, _ *http.Request) {
	a := v.archiveOr503(w)
	if a == nil {
		return
	}
	s, err := a.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (v *Viewer) handleSessions(w http.ResponseWriter, r *http.Request) {
	a := v.archiveOr503(w)
	if a == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	sessions, err := a.Sessions(limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []SessionSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (v *Viewer) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	a := v.archiveOr503(w)
	if a == nil {
		return
	}
	id := sanitizeID(r.PathValue("id"))
	recs, err := a.List(SearchOptions{SessionID: id, Limit: 500})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if recs == nil {
		recs = []Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "requests": recs})
}

func (v *Viewer) handleRequests(w http.ResponseWriter, r *http.Request) {
	a := v.archiveOr503(w)
	if a == nil {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	opt := SearchOptions{
		Query:     q.Get("q"),
		SessionID: sanitizeID(q.Get("session")),
		Action:    q.Get("action"),
		Limit:     limit,
		Offset:    offset,
	}
	recs, err := a.List(opt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	total, err := a.SearchCount(opt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if recs == nil {
		recs = []Record{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": recs, "total": total})
}

func (v *Viewer) handleRequest(w http.ResponseWriter, r *http.Request) {
	a := v.archiveOr503(w)
	if a == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad id"})
		return
	}
	rec, err := a.Get(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if rec == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (v *Viewer) handleUsage(w http.ResponseWriter, _ *http.Request) {
	a := v.archiveOr503(w)
	if a == nil {
		return
	}
	s, err := a.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if s.TopModels == nil {
		s.TopModels = []ModelUsage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens":  s.InputTokens,
		"output_tokens": s.OutputTokens,
		"cost_micro":    s.CostMicro,
		"models":        s.TopModels,
	})
}

// handleUI serves the embedded single-page app.
func (v *Viewer) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/view" && r.URL.Path != "/view/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(viewerIndexHTML)
}
