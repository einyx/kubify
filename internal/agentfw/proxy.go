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
	"sync/atomic"
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
	// Base-URL mode: origin-form requests (client pointed e.g.
	// ANTHROPIC_BASE_URL at agentfw) carry no absolute target. Resolve one
	// before scanning so the SSRF/DLP pipeline sees the real destination,
	// not the agentfw address.
	if err := px.resolveTarget(r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	r, err := px.scanner.InspectRequest(r)
	if err != nil {
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
			// Failed upstream fetches must stay traceable in the archive,
			// same as blocked requests.
			if px.scanner != nil {
				if px.scanner.Auditor != nil {
					px.scanner.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "error", Error: err.Error()})
				}
				if cap := captureFrom(r); cap != nil && px.scanner.Archive != nil {
					px.scanner.insert(*cap, "error", http.StatusBadGateway, "agentfw proxy: "+err.Error(), nil)
				}
			}
			http.Error(w, fmt.Sprintf("agentfw proxy: %v", err), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// upstreamHeader names an explicit base-URL-mode routing hint. It is stripped
// before forwarding so it never leaks to the destination.
const upstreamHeader = "X-Agentfw-Upstream"

// resolveTarget rewrites origin-form requests (base-URL mode) into absolute
// forward-proxy targets. Absolute-form requests pass through untouched.
// Resolution order:
//  1. X-Agentfw-Upstream header (host or full base URL)
//  2. target embedded in the path: /https://api.anthropic.com/v1/messages
//  3. policy baseURLRoutes keyed by the inbound Host header
//  4. policy baseURLDefault
func (px *Proxy) resolveTarget(r *http.Request) error {
	if r.URL.Host != "" {
		return nil // forward-proxy (absolute-form) request
	}
	u := px.upstreamFromHeader(r)
	if u == nil {
		u, r.URL.Path = upstreamFromPath(r.URL.Path)
	}
	if u == nil {
		u = px.upstreamFromPolicy(r.Host)
	}
	if u == nil {
		return fmt.Errorf(
			"agentfw: base-URL request %s %q has no upstream — set the %s header, embed the target in the path (/https://host/...), or set baseURLRoutes/baseURLDefault in the agentfw policy",
			r.Method, r.URL.Path, upstreamHeader)
	}
	r.URL.Scheme = u.Scheme
	r.URL.Host = u.Host
	r.Host = u.Host
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		r.URL.Path = p + r.URL.Path
	}
	r.Header.Del(upstreamHeader) // routing hint must not leak upstream
	return nil
}

func (px *Proxy) upstreamFromHeader(r *http.Request) *url.URL {
	v := strings.TrimSpace(r.Header.Get(upstreamHeader))
	if v == "" {
		return nil
	}
	u, err := parseBaseURL(v)
	if err != nil {
		return nil
	}
	return u
}

// upstreamFromPath extracts a target embedded as the entire path prefix,
// e.g. /https://api.anthropic.com/v1/messages. Returns the target URL and
// the remaining request path.
func upstreamFromPath(path string) (*url.URL, string) {
	for _, scheme := range []string{"https", "http"} {
		prefix := "/" + scheme + "://"
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		host, remainder, _ := strings.Cut(strings.TrimPrefix(path, prefix), "/")
		if host == "" {
			return nil, path
		}
		return &url.URL{Scheme: scheme, Host: host}, "/" + strings.TrimLeft(remainder, "/")
	}
	return nil, path
}

func (px *Proxy) upstreamFromPolicy(host string) *url.URL {
	if px.scanner == nil {
		return nil
	}
	p := px.scanner.Policy
	if len(p.BaseURLRoutes) > 0 && host != "" {
		if base, ok := p.BaseURLRoutes[host]; ok {
			if u, err := parseBaseURL(base); err == nil {
				return u
			}
		}
		if h, _, err := net.SplitHostPort(host); err == nil {
			if base, ok := p.BaseURLRoutes[h]; ok {
				if u, err := parseBaseURL(base); err == nil {
					return u
				}
			}
		}
	}
	if p.BaseURLDefault != "" {
		if u, err := parseBaseURL(p.BaseURLDefault); err == nil {
			return u
		}
	}
	return nil
}

// parseBaseURL accepts a host or a full base URL, defaulting to https.
func parseBaseURL(s string) (*url.URL, error) {
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("agentfw: invalid base URL %q", s)
	}
	return u, nil
}

func (px *Proxy) handleTunnel(w http.ResponseWriter, r *http.Request) {
	// 0. Kill switch — deny-all before any other check (CONNECT tunnels
	// otherwise bypass the scanner entirely in raw mode).
	if px.scanner.KillSwitch != nil && px.scanner.KillSwitch.Tripped() {
		px.traceTunnel(r, "block", http.StatusForbidden,
			"agentfw: kill switch active — all traffic blocked",
			[]Finding{{Kind: "killswitch", Pattern: "deny-all"}})
		http.Error(w, "agentfw: kill switch active — all traffic blocked", http.StatusForbidden)
		return
	}

	if px.scanner.Policy.BlockPrivateEgress && IsPrivateHost(r.Host) {
		px.traceTunnel(r, "block", http.StatusForbidden,
			"agentfw: CONNECT to private address blocked",
			[]Finding{{Kind: "ssrf", Pattern: "private-egress", Excerpt: r.Host}})
		http.Error(w, "agentfw: CONNECT to private address blocked", http.StatusForbidden)
		return
	}

	if err := px.dnsCache.Check(r.Host); err != nil {
		px.traceTunnel(r, "block", http.StatusForbidden, err.Error(),
			[]Finding{{Kind: "ssrf", Pattern: "dns-rebinding", Excerpt: err.Error()}})
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	if px.mitm != nil && !MatchesBypass(r.Host, px.scanner.Policy.MITMBypass) {
		px.tunnelMITM(w, r)
		return
	}
	px.tunnelRaw(w, r)
}

// traceTunnel records a CONNECT-level decision in both the audit stream and
// the viewer archive, so tunnel verdicts stay as traceable as HTTP calls.
func (px *Proxy) traceTunnel(r *http.Request, action string, status int, body string, findings []Finding) {
	if px.scanner == nil {
		return
	}
	if px.scanner.Auditor != nil {
		ev := Event{Method: r.Method, URL: r.Host, Action: action, Findings: findings}
		if action == "error" {
			ev.Error = body
		}
		px.scanner.Auditor.Log(ev)
	}
	a := px.scanner.Archive
	if a == nil {
		return
	}
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	rec := Record{
		SessionID: SessionID(r),
		Time:      time.Now(),
		Method:    r.Method,
		URL:       r.Host,
		Host:      host,
		Status:    status,
		Action:    action,
		RespBody:  body,
		Findings:  findings,
	}
	go func() { _, _ = a.Insert(rec) }() // best-effort, like Scanner.record
}

func (px *Proxy) tunnelRaw(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		px.traceTunnel(r, "error", http.StatusBadGateway, err.Error(), nil)
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

	// Trace the opaque tunnel: policy checks above passed, so the CONNECT
	// itself is an "allow" in the audit stream even though the payload is
	// end-to-end encrypted and cannot be scanned.
	if px.scanner.Auditor != nil {
		px.scanner.Auditor.Log(Event{Method: r.Method, URL: r.Host, Action: "allow"})
	}

	var sent, recv atomic.Int64
	done := make(chan struct{}, 2)
	cp := func(a, b net.Conn, counter *atomic.Int64) {
		n, _ := io.Copy(a, b)
		counter.Add(n)
		done <- struct{}{}
	}
	go cp(dst, conn, &recv) // upstream → client
	go cp(conn, dst, &sent) // client → upstream
	<-done

	// Archive the tunnel with byte counts and lifetime so CONNECT traffic
	// is traceable in the viewer alongside plain HTTP calls.
	if a := px.scanner.Archive; a != nil {
		host := r.Host
		if i := strings.IndexByte(host, ':'); i >= 0 {
			host = host[:i]
		}
		rec := Record{
			SessionID:  SessionID(r),
			Time:       start,
			Method:     r.Method,
			URL:        r.Host,
			Host:       host,
			Status:     http.StatusOK,
			DurationMS: time.Since(start).Milliseconds(),
			ReqBytes:   int(sent.Load()),
			RespBytes:  int(recv.Load()),
			Action:     "allow",
			Findings:   []Finding{{Kind: "tunnel", Pattern: "connect", Excerpt: "opaque TLS tunnel — payload not scanned"}},
		}
		go func() { _, _ = a.Insert(rec) }() // best-effort, like Scanner.record
	}
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
		px.traceTunnel(r, "error", 0, err.Error(),
			[]Finding{{Kind: "mitm", Pattern: "client-handshake", Excerpt: err.Error()}})
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

		req, err = px.scanner.InspectRequest(req)
		if err != nil {
			writeErr(tlsConn, http.StatusForbidden, err.Error())
			return
		}

		resp, err := upstream.Do(req)
		if err != nil {
			// Failed upstream fetches must stay traceable in the archive,
			// same as blocked requests.
			if px.scanner.Auditor != nil {
				px.scanner.Auditor.Log(Event{Method: req.Method, URL: req.URL.String(), Action: "error", Error: err.Error()})
			}
			if cap := captureFrom(req); cap != nil && px.scanner.Archive != nil {
				px.scanner.insert(*cap, "error", http.StatusBadGateway, "agentfw mitm: "+err.Error(), nil)
			}
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
