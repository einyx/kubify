package agentfw

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// ReverseProxy forwards requests to the configured upstream LLM, scanning
// both the outbound request and inbound response.
type ReverseProxy struct {
	upstream *url.URL
	rp       *httputil.ReverseProxy
	scanner  *Scanner // exported for kill switch wiring in server.go
}

func NewReverseProxy(upstream string, p Policy, a *Auditor) (*ReverseProxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("agentfw: invalid upstream %q: %w", upstream, err)
	}
	s := &Scanner{Policy: p, Auditor: a}
	rv := &ReverseProxy{upstream: u, scanner: s}
	rv.rp = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme = u.Scheme
			r.URL.Host = u.Host
			r.Host = u.Host
			// preserve path — agent calls /v1/chat/completions, we forward as-is
		},
		ModifyResponse: func(resp *http.Response) error {
			return s.InspectResponse(resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("agentfw upstream: %v", err), http.StatusBadGateway)
		},
	}
	return rv, nil
}

func (rv *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := rv.scanner.InspectRequest(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	rv.rp.ServeHTTP(w, r)
}
