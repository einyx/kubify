package agentfw

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// Proxy is the HTTP forward proxy.
type Proxy struct {
	scanner *Scanner // exported for kill switch wiring in server.go
}

func NewProxy(p Policy, a *Auditor) *Proxy {
	return &Proxy{scanner: &Scanner{Policy: p, Auditor: a}}
}

func (px *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		px.handleTunnel(w, r)
		return
	}
	px.handleHTTP(w, r)
}

func (px *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if err := px.scanner.InspectRequest(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	target := &url.URL{Scheme: r.URL.Scheme, Host: r.URL.Host}
	if target.Scheme == "" {
		target.Scheme = "http"
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			return px.scanner.InspectResponse(resp)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("agentfw proxy: %v", err), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

func (px *Proxy) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if px.scanner.Policy.BlockPrivateEgress && IsPrivateHost(r.Host) {
		px.scanner.Auditor.Log(Event{
			Method:   r.Method,
			URL:      r.Host,
			Action:   "block",
			Findings: []Finding{{Kind: "ssrf", Pattern: "private-egress", Excerpt: r.Host}},
		})
		http.Error(w, "agentfw: CONNECT to private address blocked", http.StatusForbidden)
		return
	}

	dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer dst.Close()

	w.WriteHeader(http.StatusOK)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "agentfw: hijack not supported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	done := make(chan struct{}, 2)
	cp := func(a, b net.Conn) {
		io.Copy(a, b) //nolint:errcheck
		done <- struct{}{}
	}
	go cp(dst, conn)
	go cp(conn, dst)
	<-done
}
