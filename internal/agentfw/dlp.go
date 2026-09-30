package agentfw

import (
	"regexp"
	"strings"
)

// dlpPattern is one secret detector. name is used in audit/redaction placeholders.
type dlpPattern struct {
	name string
	re   *regexp.Regexp
}

// ponytail: 45 patterns; add from agent-egress-bench corpus when new providers appear.
var dlpPatterns = []dlpPattern{
	// AWS
	{"aws-access-key", regexp.MustCompile(`(?i)(AKIA|ABIA|ACCA|ASIA)[A-Z0-9]{16}`)},
	{"aws-secret-key", regexp.MustCompile(`(?i)aws.{0,20}secret.{0,20}['\"]([A-Za-z0-9/+=]{40})['\"]`)},
	// GitHub
	{"github-token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{36,255}`)},
	{"github-fine", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}`)},
	// Google
	{"google-api-key", regexp.MustCompile(`AIza[0-9A-Za-z\-_]{35}`)},
	{"google-oauth", regexp.MustCompile(`ya29\.[0-9A-Za-z\-_]+`)},
	{"gcp-service-account", regexp.MustCompile(`"private_key_id"\s*:\s*"[0-9a-f]{40}"`)},
	// Slack
	{"slack-token", regexp.MustCompile(`xox[baprs]-[0-9A-Za-z\-]{10,48}`)},
	// Stripe
	{"stripe-key", regexp.MustCompile(`sk_(live|test)_[0-9a-zA-Z]{24,}`)},
	{"stripe-restricted", regexp.MustCompile(`rk_(live|test)_[0-9a-zA-Z]{24,}`)},
	// Twilio / SendGrid
	{"twilio-sid", regexp.MustCompile(`AC[0-9a-fA-F]{32}`)},
	{"sendgrid", regexp.MustCompile(`SG\.[a-zA-Z0-9\-_]{22}\.[a-zA-Z0-9\-_]{43}`)},
	// Azure
	{"azure-sub", regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)},
	{"azure-storage-key", regexp.MustCompile(`DefaultEndpointsProtocol=https;AccountName=[^;]+;AccountKey=[A-Za-z0-9+/=]{88}`)},
	{"azure-sas-token", regexp.MustCompile(`sv=\d{4}-\d{2}-\d{2}&s[a-z]=.{10,}&sig=[A-Za-z0-9%+/=]{40,}`)},
	// Anthropic / OpenAI / HuggingFace / Databricks / Cloudflare
	{"anthropic-key", regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{40,}`)},
	{"openai-key", regexp.MustCompile(`sk-(proj-)?[A-Za-z0-9]{48,}`)},
	{"huggingface-token", regexp.MustCompile(`hf_[A-Za-z0-9]{34,}`)},
	{"databricks-token", regexp.MustCompile(`dapi[a-f0-9]{32}`)},
	{"cloudflare-token", regexp.MustCompile(`(?i)cloudflare.{0,20}(api[_-]?token|key)\s*[:=]\s*[A-Za-z0-9_\-]{40}`)},
	// Private keys (PEM blocks)
	{"private-key-block", regexp.MustCompile(`-----BEGIN (RSA |EC |DSA |OPENSSH |PKCS8 )?PRIVATE KEY-----`)},
	{"ssh-private-key", regexp.MustCompile(`-----BEGIN OPENSSH PRIVATE KEY-----`)},
	// HTTP auth headers
	{"bearer-token", regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+[A-Za-z0-9\-._~+/]+=*`)},
	{"basic-auth", regexp.MustCompile(`(?i)Authorization:\s*Basic\s+[A-Za-z0-9+/]+=*`)},
	// Credentials in fields
	{"password-field", regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[:=]\s*\S{8,}`)},
	{"generic-secret", regexp.MustCompile(`(?i)(secret|api[_-]?key|token)\s*[:=]\s*['\"]?[A-Za-z0-9\-_]{16,}['\"]?`)},
	// Package managers / runtime tokens
	{"npm-token", regexp.MustCompile(`npm_[A-Za-z0-9]{36}`)},
	{"vault-token", regexp.MustCompile(`s\.[A-Za-z0-9]{24}`)},
	// JWT
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9\-_]+\.eyJ[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+`)},
	// Database DSNs with embedded credentials
	{"postgres-dsn", regexp.MustCompile(`postgres(ql)?://[^:]+:[^@]{6,}@`)},
	{"mysql-dsn", regexp.MustCompile(`mysql://[^:]+:[^@]{6,}@`)},
	{"redis-dsn", regexp.MustCompile(`redis://:([^@]{6,})@`)},
	// Docker
	{"docker-auth", regexp.MustCompile(`"auth"\s*:\s*"[A-Za-z0-9+/=]{20,}"`)},
	// Hash-adjacent secrets (hex secrets next to key labels)
	{"hex-secret-32", regexp.MustCompile(`(?i)(key|secret|token|seed)\s*[:=]\s*['\"]?[0-9a-f]{32}['\"]?`)},
	{"hex-secret-64", regexp.MustCompile(`(?i)(key|secret|token|seed)\s*[:=]\s*['\"]?[0-9a-f]{64}['\"]?`)},
	// Miscellaneous
	{"pypi-token", regexp.MustCompile(`pypi-[A-Za-z0-9\-_]{50,}`)},
	{"heroku-key", regexp.MustCompile(`(?i)heroku.{0,20}['\"][0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}['\"]`)},
	{"terraform-token", regexp.MustCompile(`[a-z0-9]{14}\.atlasv1\.[a-z0-9]{60,}`)},
	{"linear-key", regexp.MustCompile(`lin_api_[A-Za-z0-9]{40}`)},
	{"age-secret-key", regexp.MustCompile(`AGE-SECRET-KEY-1[A-Z0-9]{58}`)},
}

// privateIPRe matches RFC-1918 / loopback / link-local destinations.
var privateIPRe = regexp.MustCompile(
	`^(127\.|10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.|169\.254\.|::1|fc00:|fd)`)

// ScanDLP scans text for secrets and returns findings. Each finding names the
// pattern and the (redacted) matched span.
func ScanDLP(text string) []Finding {
	var out []Finding
	for _, p := range dlpPatterns {
		locs := p.re.FindAllStringIndex(text, -1)
		for _, loc := range locs {
			out = append(out, Finding{
				Kind:    "dlp",
				Pattern: p.name,
				Excerpt: redact(text[loc[0]:loc[1]]),
			})
		}
	}
	return out
}

// Redact replaces all DLP matches in text with typed placeholders.
func Redact(text string) string {
	for _, p := range dlpPatterns {
		text = p.re.ReplaceAllStringFunc(text, func(m string) string {
			return "[REDACTED:" + strings.ToUpper(p.name) + "]"
		})
	}
	return text
}

// redact keeps first 4 chars and masks the rest.
func redact(s string) string {
	if len(s) <= 4 {
		return "****"
	}
	return s[:4] + strings.Repeat("*", len(s)-4)
}

// IsPrivateHost reports whether host (host:port or bare host) is a private/loopback address.
func IsPrivateHost(host string) bool {
	// strip port
	h := host
	if i := strings.LastIndex(host, ":"); i > 0 {
		h = host[:i]
	}
	h = strings.Trim(h, "[]")
	return privateIPRe.MatchString(h)
}
