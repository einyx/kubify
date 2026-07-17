package agentfw

import "regexp"

// injectionPatterns detects prompt injection attempts in inbound response bodies.
// ponytail: 25 patterns; expand from agent-egress-bench corpus if evasion appears.
var injectionPatterns = []dlpPattern{
	// Direct instruction override
	{"ignore-previous", regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|prior|above)\s+instructions?`)},
	{"disregard", regexp.MustCompile(`(?i)disregard\s+(all\s+)?(previous|prior|above|your)\s+instructions?`)},
	{"new-instructions", regexp.MustCompile(`(?i)your\s+new\s+instructions?\s+(are|is)\s*:`)},
	{"forget-instructions", regexp.MustCompile(`(?i)forget\s+(everything|all|your)\s+(you\s+know|instructions?|context)`)},
	{"system-override", regexp.MustCompile(`(?i)\[system\]\s*:?\s*(override|ignore|new\s+instruction)`)},
	// Jailbreak patterns
	{"jailbreak-dan", regexp.MustCompile(`(?i)\bDAN\b.{0,40}(mode|enabled|activated)`)},
	{"act-as", regexp.MustCompile(`(?i)(you\s+are\s+now|act\s+as|pretend\s+(to\s+be|you\s+are))\s+.{0,60}(no\s+restriction|unfiltered|without\s+limit)`)},
	{"developer-mode", regexp.MustCompile(`(?i)developer\s+mode\s+(enabled|activated|on)`)},
	{"grandmother-trick", regexp.MustCompile(`(?i)pretend\s+(you\s+are\s+)?my\s+(grand(ma|mother|pa|father)|deceased).{0,60}(used\s+to|would\s+tell)`)},
	// Exfiltration instructions
	{"exfil-instruct", regexp.MustCompile(`(?i)(send|leak|exfiltrate|transmit)\s+(all\s+)?(your\s+)?(context|conversation|system\s+prompt|secret)`)},
	{"repeat-prompt", regexp.MustCompile(`(?i)(repeat|print|output|reveal)\s+(your\s+)?(system\s+prompt|instructions?|initial\s+prompt)`)},
	// Encoding-based evasion
	{"base64-inject", regexp.MustCompile(`(?i)base64.{0,20}decode.{0,60}(eval|exec|run|execute)`)},
	{"zero-width-chars", regexp.MustCompile(`[\x{200b}\x{200c}\x{200d}\x{2060}\x{feff}]`)},
	{"ansi-escape", regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)},
	// Token/role smuggling
	{"token-smuggle", regexp.MustCompile(`<\|.{0,30}\|>`)},
	{"role-override", regexp.MustCompile(`(?i)(assistant|system|user)\s*:\s*(ignore|override|new\s+task)`)},
	{"assistant-prefix", regexp.MustCompile(`(?i)^\s*assistant\s*:`)},
	// Tool-result poisoning
	{"tool-result-inject", regexp.MustCompile(`(?i)<tool_result>.{0,200}(ignore|override|new\s+instructions?).{0,200}</tool_result>`)},
	{"function-call-inject", regexp.MustCompile(`(?i)<function_calls>.{0,400}(ignore|override|exfiltrate).{0,400}</function_calls>`)},
	// HTML/Markdown comment injection
	{"html-comment-inject", regexp.MustCompile(`<!--.{0,200}(ignore|instruction|override|prompt).{0,200}-->`)},
	{"markdown-hidden", regexp.MustCompile(`\[.{1,80}\]\(\s*javascript:|data:text/html`)},
	// Indirect injection via URLs
	{"url-inject", regexp.MustCompile(`(?i)(fetch|load|visit|open|navigate\s+to)\s+https?://[^\s]{10,}.{0,40}(instruction|prompt|command)`)},
	// Alt-text / title injection
	{"img-alt-inject", regexp.MustCompile(`(?i)<img\s[^>]{0,200}alt\s*=\s*['"]\s*(ignore|override|new\s+instruction)`)},
	// Homoglyph detection (Cyrillic lookalikes for common Latin letters)
	{"homoglyph", regexp.MustCompile(`[\x{0430}\x{0435}\x{043e}\x{0440}\x{0441}\x{0443}\x{0445}]{3,}`)},
}

// ScanInjection checks text (typically an HTTP response body or MCP tool result)
// for prompt injection patterns.
func ScanInjection(text string) []Finding {
	var out []Finding
	for _, p := range injectionPatterns {
		if p.re.MatchString(text) {
			out = append(out, Finding{
				Kind:    "injection",
				Pattern: p.name,
				Excerpt: firstMatch(p.re, text),
			})
		}
	}
	return out
}

func firstMatch(re *regexp.Regexp, text string) string {
	m := re.FindString(text)
	if len(m) > 80 {
		m = m[:80] + "…"
	}
	return m
}
