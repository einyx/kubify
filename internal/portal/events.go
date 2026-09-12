package portal

import (
	"context"
	"net/http"

	"github.com/einyx/kubo/api/v1alpha1"
)

// startStackWatch watches Stack resources and signals every subscribed
// browser (SSE) whenever any Stack changes. Browsers respond by re-fetching
// their REST data — the SSE channel is a change signal, not a data channel.
// Runs once per Portal; safe to call concurrently.
func (p *Portal) startStackWatch(ctx context.Context) {
	p.stackWatchMu.Lock()
	defer p.stackWatchMu.Unlock()
	if p.stackWatchStarted || p.watchClient == nil {
		return
	}
	p.stackWatchStarted = true
	p.stackWatchers = map[chan struct{}]struct{}{}

	go func() {
		w, err := p.watchClient.Watch(ctx, &v1alpha1.StackList{})
		if err != nil {
			return // browsers fall back to the refresh interval
		}
		for range w.ResultChan() {
			p.notifyStackWatchers()
		}
	}()
}

// notifyStackWatchers wakes every subscribed browser. Non-blocking: a slow
// browser misses intermediate signals and refreshes on the next one.
func (p *Portal) notifyStackWatchers() {
	p.stackWatchMu.Lock()
	defer p.stackWatchMu.Unlock()
	for ch := range p.stackWatchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// stackEvents is the SSE endpoint: one "refresh" event per Stack change.
// Browsers re-fetch REST data on signal; EventSource auto-reconnects on
// drop, so transient proxy failures self-heal.
func (p *Portal) stackEvents(w http.ResponseWriter, r *http.Request) {
	p.serveStackEvents(w, r)
}

// serveStackEvents streams the SSE change signal.
func (p *Portal) serveStackEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Streaming unsupported: fail fast so the browser's EventSource
		// errors and the UI falls back to interval refresh.
		http.Error(w, "streaming unsupported", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan struct{}, 8)
	p.stackWatchMu.Lock()
	if p.stackWatchers == nil {
		p.stackWatchers = map[chan struct{}]struct{}{}
	}
	p.stackWatchers[ch] = struct{}{}
	p.stackWatchMu.Unlock()
	defer func() {
		p.stackWatchMu.Lock()
		delete(p.stackWatchers, ch)
		p.stackWatchMu.Unlock()
	}()

	// Initial signal so a freshly-connected view renders immediately.
	ch <- struct{}{}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if _, err := w.Write([]byte("event: refresh\ndata: {}\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
