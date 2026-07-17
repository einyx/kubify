package agentfw

import (
	"net/http"
	"os"
	"sync/atomic"
)

// KillSwitch is a 3-source emergency deny-all.
// Sources: file presence, env var, HTTP POST to admin endpoint.
type KillSwitch struct {
	filePath string
	tripped  atomic.Bool
}

func NewKillSwitch(filePath string) *KillSwitch {
	ks := &KillSwitch{filePath: filePath}
	// Check env at startup.
	if os.Getenv("AGENTFW_KILL") == "1" {
		ks.tripped.Store(true)
	}
	return ks
}

// Tripped reports whether the kill switch is active.
// File source is checked on every call (hot-path, but stat is cheap).
func (ks *KillSwitch) Tripped() bool {
	if ks.tripped.Load() {
		return true
	}
	if ks.filePath != "" {
		if _, err := os.Stat(ks.filePath); err == nil {
			ks.tripped.Store(true)
			return true
		}
	}
	return false
}

// Trip activates the kill switch programmatically (HTTP source).
func (ks *KillSwitch) Trip() { ks.tripped.Store(true) }

// Reset deactivates the kill switch (removes file + clears flag).
func (ks *KillSwitch) Reset() {
	ks.tripped.Store(false)
	if ks.filePath != "" {
		os.Remove(ks.filePath) //nolint:errcheck
	}
}

// AdminHandler serves the kill switch admin endpoint on a separate port.
// POST /kill → trip; DELETE /kill → reset; GET /kill → status.
func (ks *KillSwitch) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/kill", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			ks.Trip()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"tripped"}`)) //nolint:errcheck
		case http.MethodDelete:
			ks.Reset()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"reset"}`)) //nolint:errcheck
		case http.MethodGet:
			if ks.Tripped() {
				w.Write([]byte(`{"status":"tripped"}`)) //nolint:errcheck
			} else {
				w.Write([]byte(`{"status":"ok"}`)) //nolint:errcheck
			}
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}
