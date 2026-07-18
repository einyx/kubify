package agentfw

import (
	"net/http"
	"sync"
	"time"
)

// taintWeight assigns a cumulative score per injection pattern severity.
var taintWeight = map[string]int{
	// high-severity patterns
	"exfil-instruct": 4, "repeat-prompt": 4, "system-override": 4,
	"tool-result-inject": 4, "function-call-inject": 4,
	// medium
	"ignore-previous": 2, "disregard": 2, "new-instructions": 2,
	"forget-instructions": 2, "role-override": 2, "act-as": 2,
	"assistant-prefix": 2, "jailbreak-dan": 3, "developer-mode": 3,
	// low
	"base64-inject": 1, "token-smuggle": 1, "zero-width-chars": 1,
	"ansi-escape": 1, "html-comment-inject": 1, "homoglyph": 1,
}

const (
	taintLogThreshold   = 3
	taintBlockThreshold = 7
	sessionTTL          = 30 * time.Minute
)

type sessionEntry struct {
	taint      int
	boundTools map[string]bool // MCP tool binding; nil = not yet bound
	lastSeen   time.Time
	mu         sync.Mutex
}

// SessionStore tracks per-session taint and MCP tool bindings.
type SessionStore struct {
	m sync.Map
}

func NewSessionStore() *SessionStore {
	s := &SessionStore{}
	go s.reap()
	return s
}

func (s *SessionStore) get(id string) *sessionEntry {
	v, _ := s.m.LoadOrStore(id, &sessionEntry{lastSeen: time.Now()})
	e := v.(*sessionEntry)
	e.mu.Lock()
	e.lastSeen = time.Now()
	e.mu.Unlock()
	return e
}

// AddFindings records injection findings for the session and returns the taint action.
// Returns "block" if the session is fully tainted, "log" if elevated, "" if clean.
func (s *SessionStore) AddFindings(id string, findings []Finding) string {
	if len(findings) == 0 {
		return ""
	}
	e := s.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range findings {
		if f.Kind == "injection" {
			e.taint += taintWeight[f.Pattern] + 1 // +1 default for unknown patterns
		}
	}
	if e.taint > taintBlockThreshold {
		return "block"
	}
	if e.taint > taintLogThreshold {
		return "log"
	}
	return ""
}

// BindTools records the allowed MCP tools for a session on first call.
// Subsequent calls return false if the requested tool wasn't in the initial set.
func (s *SessionStore) BindTools(id string, policy Policy, requestedTool string) bool {
	e := s.get(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.boundTools == nil {
		// First request: bind to current policy tools
		e.boundTools = make(map[string]bool)
		for _, t := range policy.AllowedMCPTools {
			e.boundTools[t] = true
		}
	}
	if e.boundTools["*"] {
		return true
	}
	return e.boundTools[requestedTool]
}

// SessionID extracts a session identifier from the request.
func SessionID(r *http.Request) string {
	if id := r.Header.Get("X-Session-ID"); id != "" {
		return id
	}
	if c, err := r.Cookie("session"); err == nil && c.Value != "" {
		return "cookie:" + c.Value
	}
	return "ip:" + r.RemoteAddr
}

// reap removes stale sessions every 5 minutes.
func (s *SessionStore) reap() {
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		now := time.Now()
		s.m.Range(func(k, v any) bool {
			e := v.(*sessionEntry)
			e.mu.Lock()
			stale := now.Sub(e.lastSeen) > sessionTTL
			e.mu.Unlock()
			if stale {
				s.m.Delete(k)
			}
			return true
		})
	}
}
