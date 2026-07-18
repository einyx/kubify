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

// Auditor writes structured JSON events, optionally signing each line.
type Auditor struct {
	w      io.Writer
	signer *Signer // optional; nil = no signing
}

func NewAuditor(w io.Writer) *Auditor { return &Auditor{w: w} }

func (a *Auditor) WithSigner(s *Signer) *Auditor { a.signer = s; return a }

func (a *Auditor) Log(ev Event) {
	ev.Time = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(ev)
	if err != nil {
		log.Printf("agentfw audit marshal: %v", err)
		return
	}
	if a.signer != nil {
		// Append sig field by re-marshaling with signature.
		type signed struct {
			Event
			Sig string `json:"sig"`
		}
		sb, _ := json.Marshal(signed{Event: ev, Sig: a.signer.Sign(b)})
		b = sb
	}
	_, _ = a.w.Write(append(b, '\n'))
}
