package agentfw

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

// Scanner runs the ordered inspection pipeline on a request/response pair.
type Scanner struct {
	Policy      Policy
	Auditor     *Auditor
	KillSwitch  *KillSwitch // optional
}

// InspectRequest checks an outbound request. Returns an error if it should be blocked.
func (s *Scanner) InspectRequest(r *http.Request) error {
	var findings []Finding

	// 0. Kill switch — deny-all before any other check
	if s.KillSwitch != nil && s.KillSwitch.Tripped() {
		s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "block",
			Findings: []Finding{{Kind: "killswitch", Pattern: "deny-all"}}})
		return fmt.Errorf("agentfw: kill switch active — all traffic blocked")
	}

	// 1. SSRF floor: block private/loopback destinations
	if s.Policy.BlockPrivateEgress && IsPrivateHost(r.Host) {
		s.Auditor.Log(Event{
			Method:   r.Method,
			URL:      r.URL.String(),
			Action:   "block",
			Findings: []Finding{{Kind: "ssrf", Pattern: "private-egress", Excerpt: r.Host}},
		})
		return fmt.Errorf("agentfw: blocked private egress to %s", r.Host)
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
	if r.Body != nil && r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
		r.Body.Close()
		if err == nil {
			bodyStr := string(body)
			bf := ScanDLP(bodyStr)
			findings = append(findings, bf...)
			if s.Policy.DLPAction == "block" && len(bf) > 0 {
				s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: "block", Findings: findings})
				return fmt.Errorf("agentfw: DLP block on request body (%d findings)", len(bf))
			}
			// redact and rewind
			r.Body = io.NopCloser(strings.NewReader(Redact(bodyStr)))
			r.ContentLength = -1
		}
	}

	action := "allow"
	if len(findings) > 0 {
		action = "redact"
	}
	s.Auditor.Log(Event{Method: r.Method, URL: r.URL.String(), Action: action, Findings: findings})
	return nil
}

// InspectResponse checks an inbound response body for prompt injection.
// The body is buffered, scanned, and replaced with a safe copy.
func (s *Scanner) InspectResponse(resp *http.Response) error {
	if resp.Body == nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	resp.Body.Close()
	if err != nil {
		return err
	}

	findings := ScanInjection(string(body))
	if len(findings) > 0 {
		s.Auditor.Log(Event{
			Method:   resp.Request.Method,
			URL:      resp.Request.URL.String(),
			Action:   s.Policy.InjectionAction,
			Findings: findings,
		})
		if s.Policy.InjectionAction == "block" {
			resp.Body = io.NopCloser(strings.NewReader(`{"error":"agentfw: response blocked (prompt injection detected)"}`))
			resp.StatusCode = http.StatusBadGateway
			resp.ContentLength = -1
			return nil
		}
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return nil
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
