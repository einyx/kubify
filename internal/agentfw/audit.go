package agentfw

import (
	"encoding/json"
	"io"
	"log"
	"time"
)

// Finding is one scanner hit.
type Finding struct {
	Kind    string `json:"kind"`    // "dlp" | "injection" | "ssrf"
	Pattern string `json:"pattern"` // pattern name
	Excerpt string `json:"excerpt"` // redacted match snippet
}

// Event is one structured audit log line.
type Event struct {
	Time     string    `json:"time"`
	Method   string    `json:"method,omitempty"`
	URL      string    `json:"url,omitempty"`
	Action   string    `json:"action"` // "allow" | "block" | "redact"
	Findings []Finding `json:"findings,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Auditor writes structured JSON events.
type Auditor struct{ w io.Writer }

func NewAuditor(w io.Writer) *Auditor { return &Auditor{w: w} }

func (a *Auditor) Log(ev Event) {
	ev.Time = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(ev)
	if err != nil {
		log.Printf("agentfw audit marshal: %v", err)
		return
	}
	_, _ = a.w.Write(append(b, '\n'))
}
