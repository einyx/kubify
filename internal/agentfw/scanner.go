package agentfw

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// ctxKeyCapture carries archive metadata from InspectRequest to
// InspectResponse through the request context.
var ctxKeyCapture struct{}

// reqCapture is the request-half of an archived record. It rides the
// request context so the response half (which may arrive via a proxy
// clone that preserves context) can complete the row.
type reqCapture struct {
	start      time.Time
	session    string
	method     string
	url        string
	host       string
	reqBody    string
	model      string
	action     string
	findings   []Finding
	sourceIP   string
	sourceName string
}

// Scanner runs the ordered inspection pipeline on a request/response pair.
type Scanner struct {
	Policy     Policy
	Auditor    *Auditor
	KillSwitch *KillSwitch     // optional
	Sessions   *SessionStore   // optional; enables taint classification
	Archive    *Archive        // optional; enables the viewer archive
	Sources    *SourceResolver // optional; persists Kubernetes workload identity
}

// InspectRequest checks an outbound request. Returns an error if it should be blocked.
// When the Archive is wired it returns a request whose context carries the
// capture metadata for InspectResponse to complete.
func (s *Scanner) InspectRequest(r *http.Request) (*http.Request, error) {
	var findings []Finding
	capture := &reqCapture{start: time.Now(), method: r.Method, url: r.URL.String(), host: r.Host}
	if s.Archive != nil {
		capture.session = SessionID(r)
		capture.sourceIP, capture.sourceName = s.Sources.Resolve(r.RemoteAddr)
	}

	// 0. Kill switch — deny-all before any other check
	if s.KillSwitch != nil && s.KillSwitch.Tripped() {
		s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "block",
			Findings: []Finding{{Kind: "killswitch", Pattern: "deny-all"}}})
		if s.Archive != nil {
			s.insert(*capture, "block", 0, "", []Finding{{Kind: "killswitch", Pattern: "deny-all"}})
		}
		return r, fmt.Errorf("agentfw: kill switch active — all traffic blocked")
	}

	// 1. SSRF floor: block private/loopback destinations
	if s.Policy.BlockPrivateEgress && IsPrivateHost(r.Host) {
		f := Finding{Kind: "ssrf", Pattern: "private-egress", Excerpt: r.Host}
		s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "block", Findings: []Finding{f}})
		if s.Archive != nil {
			s.insert(*capture, "block", 0, "", []Finding{f})
		}
		return r, fmt.Errorf("agentfw: blocked private egress to %s", r.Host)
	}

	// 2. DLP on URL (query params often carry tokens)
	urlStr := r.URL.String()
	urlFindings := ScanDLP(urlStr)
	findings = append(findings, urlFindings...)

	// 3. Entropy analysis on URL path+query (high entropy = potential exfil)
	if ef := entropyFinding(urlStr); ef != nil {
		findings = append(findings, *ef)
	}

	// 4. DLP on request body (read, scan, rewind)
	bodyStr := ""
	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
		r.Body.Close()
		if err == nil {
			bodyStr = string(body)
			bf := ScanDLP(bodyStr)
			findings = append(findings, bf...)
			if s.Policy.DLPAction == "block" && len(bf) > 0 {
				s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "block", Findings: findings})
				if s.Archive != nil {
					s.insert(*capture, "block", 0, "", findings)
				}
				return r, fmt.Errorf("agentfw: DLP block on request body (%d findings)", len(bf))
			}
			// redact and rewind
			bodyStr = Redact(bodyStr)
			r.Body = io.NopCloser(strings.NewReader(bodyStr))
			r.ContentLength = -1
		}
	}

	action := "allow"
	if len(findings) > 0 {
		action = "redact"
	}
	s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: action, Findings: findings})

	if s.Archive != nil {
		capture.reqBody = bodyStr
		capture.model = RequestModel(bodyStr)
		capture.action = action
		capture.findings = findings
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyCapture, capture))
	}
	return r, nil
}

// InspectResponse checks an inbound response body for prompt injection.
// The body is buffered, scanned, and replaced with a safe copy.
// When the request context carries a capture (Archive enabled), the
// completed record is written to the viewer archive.
func (s *Scanner) InspectResponse(resp *http.Response) error {
	if resp.Body == nil {
		return nil
	}
	scannedTotal.Inc()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	resp.Body.Close()
	if err != nil {
		return err
	}
	bodyStr := string(body)

	cap := captureFrom(resp.Request)

	// Response findings combine with the request-half findings so the
	// archived record shows the full inspection trail.
	var allFindings []Finding
	if cap != nil {
		allFindings = append(allFindings, cap.findings...)
	}
	findings := ScanInjection(bodyStr)

	// DLP on response bodies: an LLM can echo secrets from retrieved context
	// or tool output. Same action as requests — redact (default) or block.
	// Kept separate from injection findings: DLP hits follow DLPAction
	// (redact/block), they must not trip the injection-block branch.
	dlpFindings := ScanDLP(bodyStr)
	if s.Policy.DLPAction == "block" && len(dlpFindings) > 0 {
		s.Auditor.Log(Event{
			Method:   resp.Request.Method,
			URL:      resp.Request.URL.String(),
			Action:   "block",
			Findings: dlpFindings,
		})
		bodyStr = "agentfw: DLP block on response body"
		if s.Archive != nil {
			s.record(cap, resp, bodyStr, "block", append(allFindings, dlpFindings...))
		}
		resp.Header.Del("Content-Length")
		resp.Body = io.NopCloser(strings.NewReader(bodyStr))
		resp.ContentLength = int64(len(bodyStr))
		return nil
	}
	if len(dlpFindings) > 0 {
		bodyStr = Redact(bodyStr)
		s.Auditor.Log(Event{
			Method:   resp.Request.Method,
			URL:      resp.Request.URL.String(),
			Action:   "redact",
			Findings: dlpFindings,
		})
	}

	// Session taint escalation
	sessionAction := ""
	if s.Sessions != nil && resp.Request != nil {
		sid := SessionID(resp.Request)
		sessionAction = s.Sessions.AddFindings(sid, findings)
	}

	if len(findings) > 0 || sessionAction == "block" {
		action := s.Policy.InjectionAction
		if sessionAction == "block" {
			action = "block"
		}
		s.Auditor.Log(Event{
			Method:   resp.Request.Method,
			URL:      resp.Request.URL.String(),
			Action:   action,
			Findings: findings,
		})
		if action == "block" {
			resp.Body = io.NopCloser(strings.NewReader(`{"error":"agentfw: response blocked (prompt injection detected)"}`))
			resp.StatusCode = http.StatusBadGateway
			resp.ContentLength = -1
			if s.Archive != nil {
				s.record(cap, resp, `{"error":"agentfw: response blocked (prompt injection detected)"}`, "block", append(allFindings, findings...))
			}
			return nil
		}
		if s.Archive != nil {
			allFindings = append(allFindings, findings...)
		}
	}
	if s.Archive != nil && len(dlpFindings) > 0 {
		allFindings = append(allFindings, dlpFindings...)
	}

	// SVG hardening
	if err := HardenSVGResponse(resp); err != nil {
		return err
	}

	resp.Body = io.NopCloser(bytes.NewReader([]byte(bodyStr)))
	resp.ContentLength = int64(len(bodyStr))
	if s.Archive != nil {
		s.record(cap, resp, bodyStr, "", allFindings)
	}
	return nil
}

// captureFrom extracts the request-half capture from the request context.
func captureFrom(r *http.Request) *reqCapture {
	if r == nil {
		return nil
	}
	cap, _ := r.Context().Value(ctxKeyCapture).(*reqCapture)
	return cap
}

// insert archives a request that never produced a response (blocked
// before egress). action is forced to "block"; status unknown.
func (s *Scanner) insert(cap reqCapture, action string, status int, respBody string, findings []Finding) {
	rec := Record{
		SessionID:  cap.session,
		SourceIP:   cap.sourceIP,
		SourceName: cap.sourceName,
		Time:       cap.start,
		Method:     cap.method,
		URL:        cap.url,
		Host:       cap.host,
		Model:      cap.model,
		Status:     status,
		DurationMS: time.Since(cap.start).Milliseconds(),
		ReqBytes:   len(cap.reqBody),
		RespBytes:  len(respBody),
		Action:     action,
		ReqBody:    cap.reqBody,
		RespBody:   respBody,
		Findings:   findings,
	}
	// Insert is best-effort: never fail a scan because the viewer is sick.
	go func() {
		_, _ = s.Archive.Insert(rec)
	}()
}

// record archives one completed request/response pair. An empty respAction
// keeps the request-phase action (allow/redact).
func (s *Scanner) record(cap *reqCapture, resp *http.Response, respBody string, respAction string, findings []Finding) {
	if cap == nil {
		return
	}
	action := cap.action
	if severity(respAction) > severity(action) {
		action = respAction
	}
	usage := ParseUsage(respBody)
	model := usage.Model
	if model == "" {
		model = cap.model
	}
	usage.Model = model
	rec := Record{
		SessionID:  cap.session,
		SourceIP:   cap.sourceIP,
		SourceName: cap.sourceName,
		Time:       cap.start,
		Method:     cap.method,
		URL:        cap.url,
		Host:       cap.host,
		Model:      model,
		Status:     resp.StatusCode,
		DurationMS: time.Since(cap.start).Milliseconds(),
		ReqBytes:   len(cap.reqBody),
		RespBytes:  len(respBody),
		Action:     action,
		ReqBody:    cap.reqBody,
		RespBody:   respBody,
		Findings:   findings,
	}
	if usage.HasUsage {
		u := usage
		rec.Usage = &u
	}
	go func() {
		_, _ = s.Archive.Insert(rec)
	}()
}

// severity ranks actions so response-phase verdicts can only escalate.
func severity(a string) int {
	switch a {
	case "block":
		return 2
	case "redact":
		return 1
	default:
		return 0
	}
}

// entropyFinding returns a Finding if the URL has suspiciously high Shannon
// entropy (> 5.0 bits/char), which may indicate base64-encoded data exfil.
// ponytail: threshold 5.0 empirically covers b64/hex payloads; tune if noisy.
func entropyFinding(u string) *Finding {
	if len(u) < 20 {
		return nil
	}
	freq := make(map[rune]float64)
	for _, c := range u {
		freq[c]++
	}
	n := float64(len(u))
	var h float64
	for _, count := range freq {
		p := count / n
		h -= p * math.Log2(p)
	}
	if h > 5.0 {
		return &Finding{
			Kind:    "entropy",
			Pattern: "high-entropy-url",
			Excerpt: fmt.Sprintf("%.2f bits/char", h),
		}
	}
	return nil
}
