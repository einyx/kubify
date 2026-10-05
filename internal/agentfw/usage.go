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
	CacheWrite   int64  `json:"cache_write_tokens,omitempty"`
	CostMicro    int64  `json:"cost_micro,omitempty"`
	HasUsage     bool   `json:"has_usage"`

	// cacheInclusive is true when the provider counts cached reads inside
	// InputTokens (OpenAI, Google). It shapes the cost math only and is
	// deliberately not serialized.
	cacheInclusive bool
}

// price is USD per million tokens, split into input/output rates. Cache
// rates default to the provider-standard multiples (see prices.go).
type price struct {
	in, out, cacheRead, cacheWrite float64
}

// priceTable is a small builtin snapshot (USD / 1M tokens). Unknown models
// are reported with usage but no cost, like agentsview's unpriced models.
var priceTable = map[string]price{
	// OpenAI
	"gpt-4o":        {in: 2.50, out: 10.00},
	"gpt-4o-mini":   {in: 0.15, out: 0.60},
	"gpt-4.1":       {in: 2.00, out: 8.00},
	"gpt-4.1-mini":  {in: 0.40, out: 1.60},
	"gpt-4.1-nano":  {in: 0.10, out: 0.40},
	"gpt-4-turbo":   {in: 10.00, out: 30.00},
	"gpt-4":         {in: 30.00, out: 60.00},
	"gpt-3.5-turbo": {in: 0.50, out: 1.50},
	"o1":            {in: 15.00, out: 60.00},
	"o1-mini":       {in: 1.10, out: 4.40},
	"o3":            {in: 2.00, out: 8.00},
	"o3-mini":       {in: 1.10, out: 4.40},
	"o4-mini":       {in: 1.10, out: 4.40},
	// Anthropic
	"claude-opus-4":     {in: 15.00, out: 75.00},
	"claude-sonnet-4":   {in: 3.00, out: 15.00},
	"claude-3-7-sonnet": {in: 3.00, out: 15.00},
	"claude-3-5-sonnet": {in: 3.00, out: 15.00},
	"claude-3-5-haiku":  {in: 0.80, out: 4.00},
	"claude-3-opus":     {in: 15.00, out: 75.00},
	"claude-3-haiku":    {in: 0.25, out: 1.25},
	// Google
	"gemini-2.5-pro":   {in: 1.25, out: 10.00},
	"gemini-2.5-flash": {in: 0.30, out: 2.50},
	"gemini-2.0-flash": {in: 0.10, out: 0.40},
	"gemini-1.5-pro":   {in: 1.25, out: 5.00},
	"gemini-1.5-flash": {in: 0.075, out: 0.30},
	// Meta / misc via OpenAI-compatible gateways
	"llama-3.3-70b":     {in: 0.60, out: 0.60},
	"deepseek-chat":     {in: 0.27, out: 1.10},
	"deepseek-reasoner": {in: 0.55, out: 2.19},
}

// lookupPrice matches a model string to a price entry by normalized prefix,
// so dated snapshots ("claude-3-5-sonnet-20241022") resolve to the family.
// File-loaded overrides (prices.yaml) win with an exact match.
func lookupPrice(model string) (price, bool) {
	m := strings.ToLower(model)
	m = strings.TrimPrefix(m, "accounts/") // bedrock ARNs
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	priceMu.RLock()
	p, ok := priceOver[m]
	priceMu.RUnlock()
	if ok {
		return p.withDefaults(), true
	}
	if p, ok := priceTable[m]; ok {
		return p.withDefaults(), true
	}
	for name, p := range priceTable {
		if strings.HasPrefix(m, name) {
			return p.withDefaults(), true
		}
	}
	return price{}, false
}

// costMicroUSD converts token counts + a price into integer microdollars.
// cacheInclusive reports whether the provider counts cached reads inside
// the input total (OpenAI prompt_tokens and Google promptTokenCount do;
// Anthropic reports cache_read_input_tokens separately from input_tokens).
// Cache writes are always additive.
func costMicroUSD(p price, in, cacheRead, cacheWrite, out int64, cacheInclusive bool) int64 {
	const microPerMillion = 1e6
	rate := func(usd float64, tokens int64) float64 { return usd * float64(tokens) }
	if cacheInclusive && cacheRead > in {
		cacheRead = in // defensive: never go negative on malformed usage
	}
	billedIn := in
	if cacheInclusive {
		billedIn = in - cacheRead
	}
	return int64((rate(p.in, billedIn) +
		rate(p.cacheRead, cacheRead) +
		rate(p.cacheWrite, cacheWrite) +
		rate(p.out, out)) * microPerMillion / 1e6)
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
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`     // Anthropic
			CacheWriteTokens    int64 `json:"cache_creation_input_tokens"` // Anthropic
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			// Anthropic
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			// Google
			PromptTokenCount        int64 `json:"promptTokenCount"`
			CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
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
		// Anthropic messages API: input_tokens excludes cache tokens;
		// both cache counters bill additively.
		u.InputTokens = raw.Usage.InputTokens
		u.OutputTokens = raw.Usage.OutputTokens
		u.CacheRead = raw.Usage.CacheReadTokens
		u.CacheWrite = raw.Usage.CacheWriteTokens
		u.cacheInclusive = false
	case raw.Usage.PromptTokens != 0 || raw.Usage.CompletionTokens != 0:
		// OpenAI chat completions / responses API: prompt_tokens
		// includes cached tokens — split them out for the cache rate.
		u.InputTokens = raw.Usage.PromptTokens
		u.OutputTokens = raw.Usage.CompletionTokens
		if raw.Usage.PromptTokensDetails != nil {
			u.CacheRead = raw.Usage.PromptTokensDetails.CachedTokens
		}
		u.cacheInclusive = true
	case raw.UsageMetadata != nil:
		// Google generateContent: promptTokenCount includes cached
		// content, billed at a discount.
		u.InputTokens = raw.UsageMetadata.PromptTokenCount
		u.OutputTokens = raw.UsageMetadata.CandidatesTokenCount
		u.CacheRead = raw.UsageMetadata.CachedContentTokenCount
		u.cacheInclusive = true
	default:
		return Usage{}
	}
	u.HasUsage = true

	if p, ok := lookupPrice(u.Model); ok {
		u.CostMicro = costMicroUSD(p, u.InputTokens, u.CacheRead, u.CacheWrite, u.OutputTokens, u.cacheInclusive)
	} else {
		warnUnpriced(u.Model)
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
