package agentfw

import (
	"encoding/json"
	"strings"
)

// Usage is the token accounting extracted from one LLM response body.
// Cost is stored in microdollars (1e-6 USD), mirroring agentsview's
// machine-readable money convention.
type Usage struct {
	Model        string `json:"model,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CacheRead    int64  `json:"cache_read_tokens,omitempty"`
	CostMicro    int64  `json:"cost_micro,omitempty"`
	HasUsage     bool   `json:"has_usage"`
}

// price is USD per million tokens, split into input/output rates.
type price struct{ in, out float64 }

// priceTable is a small builtin snapshot (USD / 1M tokens). Unknown models
// are reported with usage but no cost, like agentsview's unpriced models.
var priceTable = map[string]price{
	// OpenAI
	"gpt-4o":            {2.50, 10.00},
	"gpt-4o-mini":       {0.15, 0.60},
	"gpt-4.1":           {2.00, 8.00},
	"gpt-4.1-mini":      {0.40, 1.60},
	"gpt-4.1-nano":      {0.10, 0.40},
	"gpt-4-turbo":       {10.00, 30.00},
	"gpt-4":             {30.00, 60.00},
	"gpt-3.5-turbo":     {0.50, 1.50},
	"o1":                {15.00, 60.00},
	"o1-mini":           {1.10, 4.40},
	"o3":                {2.00, 8.00},
	"o3-mini":           {1.10, 4.40},
	"o4-mini":           {1.10, 4.40},
	// Anthropic
	"claude-opus-4":     {15.00, 75.00},
	"claude-sonnet-4":   {3.00, 15.00},
	"claude-3-7-sonnet": {3.00, 15.00},
	"claude-3-5-sonnet": {3.00, 15.00},
	"claude-3-5-haiku":  {0.80, 4.00},
	"claude-3-opus":     {15.00, 75.00},
	"claude-3-haiku":    {0.25, 1.25},
	// Google
	"gemini-2.5-pro":     {1.25, 10.00},
	"gemini-2.5-flash":   {0.30, 2.50},
	"gemini-2.0-flash":   {0.10, 0.40},
	"gemini-1.5-pro":     {1.25, 5.00},
	"gemini-1.5-flash":   {0.075, 0.30},
	// Meta / misc via OpenAI-compatible gateways
	"llama-3.3-70b":     {0.60, 0.60},
	"deepseek-chat":     {0.27, 1.10},
	"deepseek-reasoner": {0.55, 2.19},
}

// lookupPrice matches a model string to a price entry by normalized prefix,
// so dated snapshots ("claude-3-5-sonnet-20241022") resolve to the family.
func lookupPrice(model string) (price, bool) {
	m := strings.ToLower(model)
	m = strings.TrimPrefix(m, "accounts/") // bedrock ARNs
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if p, ok := priceTable[m]; ok {
		return p, true
	}
	for name, p := range priceTable {
		if strings.HasPrefix(m, name) {
			return p, true
		}
	}
	return price{}, false
}

// costMicroUSD converts token counts + a price into integer microdollars.
func costMicroUSD(p price, in, out int64) int64 {
	const microPerMillion = 1e6
	return int64(p.in*float64(in)*microPerMillion/1e6 + p.out*float64(out)*microPerMillion/1e6)
}

// ParseUsage extracts model + token usage from an LLM response body.
// Understands OpenAI chat completions, Anthropic messages, and Google
// generateContent response shapes. Returns zero Usage (HasUsage=false)
// for anything that does not parse.
func ParseUsage(body string) Usage {
	var raw struct {
		Model string `json:"model"`
		Usage struct {
			// OpenAI
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			CacheReadTokens  int64 `json:"cache_read_input_tokens"` // Anthropic
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			// Anthropic
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			// Google
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		} `json:"usage"`
		// Google nests usageMetadata at the top level.
		UsageMetadata *struct {
			PromptTokenCount        int64 `json:"promptTokenCount"`
			CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
			CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return Usage{}
	}

	u := Usage{Model: raw.Model}
	switch {
	case raw.Usage.InputTokens != 0 || raw.Usage.OutputTokens != 0:
		// Anthropic messages API
		u.InputTokens = raw.Usage.InputTokens
		u.OutputTokens = raw.Usage.OutputTokens
		u.CacheRead = raw.Usage.CacheReadTokens
	case raw.Usage.PromptTokens != 0 || raw.Usage.CompletionTokens != 0:
		// OpenAI chat completions / responses API
		u.InputTokens = raw.Usage.PromptTokens
		u.OutputTokens = raw.Usage.CompletionTokens
		if raw.Usage.PromptTokensDetails != nil {
			u.CacheRead = raw.Usage.PromptTokensDetails.CachedTokens
		}
	case raw.UsageMetadata != nil:
		// Google generateContent
		u.InputTokens = raw.UsageMetadata.PromptTokenCount
		u.OutputTokens = raw.UsageMetadata.CandidatesTokenCount
		u.CacheRead = raw.UsageMetadata.CachedContentTokenCount
	default:
		return Usage{}
	}
	u.HasUsage = true

	if p, ok := lookupPrice(u.Model); ok {
		u.CostMicro = costMicroUSD(p, u.InputTokens, u.OutputTokens)
	}
	return u
}

// RequestModel extracts the model name from an LLM request body
// (OpenAI and Anthropic both use a top-level "model" field).
func RequestModel(body string) string {
	var raw struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return ""
	}
	return raw.Model
}
