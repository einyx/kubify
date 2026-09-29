package agentfw

import (
	"os"

	"sigs.k8s.io/yaml"
)

// Policy is loaded from the agentfw-policy ConfigMap (mounted as a file).
type Policy struct {
	// AllowedMCPTools lists permitted MCP tool names. ["*"] means allow all.
	AllowedMCPTools []string `json:"allowedMCPTools"`
	// BlockPrivateEgress blocks CONNECT/requests to RFC-1918 and loopback addresses.
	BlockPrivateEgress bool `json:"blockPrivateEgress"`
	// DLPAction is "redact" (default) or "block".
	DLPAction string `json:"dlpAction"`
	// InjectionAction is "block" (default) or "log".
	InjectionAction string `json:"injectionAction"`
	// Upstream is the LLM base URL for reverse proxy mode (e.g. https://api.openai.com).
	// When set, agentfw acts as a reverse proxy: agents call agentfw directly and
	// it forwards to upstream, scanning both directions.
	Upstream string `json:"upstream,omitempty"`
	// BaseURLRoutes maps the inbound Host header to an upstream base URL for
	// base-URL mode: clients point e.g. ANTHROPIC_BASE_URL=http://agentfw:8080
	// and send origin-form requests; agentfw resolves the destination here.
	// Keys match host or host:port.
	BaseURLRoutes map[string]string `json:"baseURLRoutes,omitempty"`
	// BaseURLDefault is the fallback upstream for base-URL requests whose
	// Host matches no baseURLRoutes entry. The X-Agentfw-Upstream header
	// and path-embedded targets (/https://host/...) always take precedence.
	BaseURLDefault string `json:"baseURLDefault,omitempty"`
	// RequestsPerMinute caps outbound requests. 0 = unlimited.
	RequestsPerMinute int `json:"requestsPerMinute,omitempty"`
	// DataBudgetMB caps total outbound bytes (resets on restart). 0 = unlimited.
	DataBudgetMB int `json:"dataBudgetMB,omitempty"`
	// SigningKeyPath is the Ed25519 private key PEM for audit receipt signing.
	// Generated and saved on first run if absent.
	SigningKeyPath string `json:"signingKeyPath,omitempty"`
	// RulesPath is a YAML file of additional DLP/injection patterns to load.
	RulesPath string `json:"rulesPath,omitempty"`
	// PricesPath overrides where the billing price table is loaded from.
	// Default /etc/agentfw/prices.yaml; entries merge over the builtin
	// snapshot so new models bill correctly without a rebuild.
	PricesPath string `json:"pricesPath,omitempty"`
	// MITMEnabled turns on TLS termination of CONNECT tunnels so response
	// scanners can see decrypted bodies. Off by default.
	MITMEnabled bool `json:"mitmEnabled,omitempty"`
	// MITMCACert / MITMCAKey are PEM paths for the CA that mints leaf certs.
	// Required when MITMEnabled. Generate with `agentfw mitm-ca`.
	MITMCACert string `json:"mitmCaCert,omitempty"`
	MITMCAKey  string `json:"mitmCaKey,omitempty"`
	// MITMBypass lists SNI hostname suffixes to tunnel opaquely
	// (cert-pinned APIs, sensitive endpoints). Matched as suffix.
	MITMBypass []string `json:"mitmBypass,omitempty"`
	// ViewDisabled turns off the persistent request archive + view UI.
	// On by default; set true for ephemeral/air-gapped deployments.
	ViewDisabled bool `json:"viewDisabled,omitempty"`
	// ViewDBPath is the SQLite archive location. Default
	// /var/lib/agentfw/view.db; override with AGENTFW_VIEW_DB or here.
	ViewDBPath string `json:"viewDBPath,omitempty"`
}

func DefaultPolicy() Policy {
	return Policy{
		AllowedMCPTools:    []string{"*"},
		BlockPrivateEgress: true,
		DLPAction:          "redact",
		InjectionAction:    "block",
	}
}

// LoadPolicy reads the policy YAML file at path. Falls back to DefaultPolicy on error.
func LoadPolicy(path string) (Policy, error) {
	p := DefaultPolicy()
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := yaml.Unmarshal(b, &p); err != nil {
		return p, err
	}
	return p, nil
}

func (p Policy) MCPToolAllowed(tool string) bool {
	for _, t := range p.AllowedMCPTools {
		if t == "*" || t == tool {
			return true
		}
	}
	return false
}
