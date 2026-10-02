package agentfw

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Proxy is the HTTP forward proxy.
type Proxy struct {
	scanner  *Scanner // exported for kill switch wiring in server.go
	dnsCache *DNSCache
	mitm     *MITM // nil disables TLS termination
}

func NewProxy(p Policy, a *Auditor) *Proxy {
	return &Proxy{
		scanner:  &Scanner{Policy: p, Auditor: a},
		dnsCache: NewDNSCache(),
	}
}

// WithMITM enables TLS termination of CONNECT tunnels.
func (px *Proxy) WithMITM(m *MITM) *Proxy { px.mitm = m; return px }

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
	// 0. Kill switch — deny-all before any other check (CONNECT tunnels
	// otherwise bypass the scanner entirely in raw mode).
	if px.scanner.KillSwitch != nil && px.scanner.KillSwitch.Tripped() {
		px.scanner.Auditor.Log(Event{
			Method:   r.Method,
			URL:      r.Host,
			Action:   "block",
			Findings: []Finding{{Kind: "killswitch", Pattern: "deny-all"}},
		})
		http.Error(w, "agentfw: kill switch active — all traffic blocked", http.StatusForbidden)
		return
	}

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

	if err := px.dnsCache.Check(r.Host); err != nil {
		px.scanner.Auditor.Log(Event{Method: r.Method, URL: r.Host, Action: "block",
			Findings: []Finding{{Kind: "ssrf", Pattern: "dns-rebinding", Excerpt: err.Error()}}})
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if px.mitm != nil && !MatchesBypass(r.Host, px.scanner.Policy.MITMBypass) {
		px.tunnelMITM(w, r)
		return
	}
	px.tunnelRaw(w, r)
}

func (px *Proxy) tunnelRaw(w http.ResponseWriter, r *http.Request) {
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

// tunnelMITM terminates the client's TLS, forwards each request to the real
// upstream over TLS, and runs the response through the normal scanner pipeline.
func (px *Proxy) tunnelMITM(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "agentfw: hijack not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	clientRaw, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientRaw.Close()

	sniHost := r.Host
	if i := strings.IndexByte(sniHost, ':'); i >= 0 {
		sniHost = sniHost[:i]
	}

	tlsConn := tls.Server(clientRaw, px.mitm.TLSConfig())
	if err := tlsConn.Handshake(); err != nil {
		px.scanner.Auditor.Log(Event{Method: r.Method, URL: r.Host, Action: "error",
			Findings: []Finding{{Kind: "mitm", Pattern: "client-handshake", Excerpt: err.Error()}}})
		return
	}
	defer tlsConn.Close()

	upstream := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: sniHost, MinVersion: tls.VersionTLS12},
			DialContext:     (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	br := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		req.URL.Scheme = "https"
		req.URL.Host = r.Host
		req.RequestURI = ""

		if err := px.scanner.InspectRequest(req); err != nil {
			writeErr(tlsConn, http.StatusForbidden, err.Error())
			return
		}

		resp, err := upstream.Do(req)
		if err != nil {
			writeErr(tlsConn, http.StatusBadGateway, "agentfw mitm: "+err.Error())
			return
		}
		if err := px.scanner.InspectResponse(resp); err != nil {
			resp.Body.Close()
			writeErr(tlsConn, http.StatusBadGateway, "agentfw mitm: "+err.Error())
			return
		}
		if err := resp.Write(tlsConn); err != nil {
			resp.Body.Close()
			return
		}
		resp.Body.Close()
		if req.Close || resp.Close {
			return
		}
	}
}

func writeErr(w io.Writer, code int, msg string) {
	body := msg + "\n"
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Length: %d\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(body), body)
}
