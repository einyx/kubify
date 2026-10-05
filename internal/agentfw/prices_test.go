package agentfw

import (
	"os"
	"path/filepath"
	"testing"
)

// Cache reads bill at the discounted cache rate, not the full input rate —
// and for inclusive providers (OpenAI/Google) they must be split out of the
// prompt total, not billed twice.
func TestCostMicroUSDCache(t *testing.T) {
	p := price{in: 3.00, out: 15.00}.withDefaults() // cacheRead=0.30, cacheWrite=3.75

	// Anthropic (exclusive): 1k in + 1k cache-read + 500 cache-write + 1k out
	got := costMicroUSD(p, 1000, 1000, 500, 1000, false)
	want := int64(3.00*1000/1e6*1e6 + 0.30*1000/1e6*1e6 + 3.75*500/1e6*1e6 + 15.00*1000/1e6*1e6)
	if got != want {
		t.Fatalf("anthropic cache cost = %d, want %d", got, want)
	}

	// OpenAI (inclusive): prompt=2k of which 1k cached — cached tokens are
	// re-billed at the cache rate, the rest at the input rate.
	incl := costMicroUSD(p, 2000, 1000, 0, 1000, true)
	excl := costMicroUSD(p, 1000, 1000, 0, 1000, false)
	if incl != excl {
		t.Fatalf("inclusive %d != exclusive %d for same real token mix", incl, excl)
	}

	// No cache tokens: identical to the legacy input/output-only math.
	legacy := int64(3.00*float64(1000) + 15.00*float64(1000))
	if got := costMicroUSD(p, 1000, 0, 0, 1000, true); got != legacy {
		t.Fatalf("no-cache cost = %d, want %d", got, legacy)
	}

	// Malformed usage (cacheRead > input) must not go negative.
	if got := costMicroUSD(p, 100, 500, 0, 0, true); got < 0 {
		t.Fatalf("negative cost on malformed usage: %d", got)
	}
}

func TestParseUsageAnthropicCache(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300}}`
	u := ParseUsage(body)
	if !u.HasUsage || u.CacheRead != 2000 || u.CacheWrite != 300 {
		t.Fatalf("usage = %+v", u)
	}
	// 3*1000 + 0.3*2000 + 3.75*300 + 15*500 = 12,225 micro-USD
	if u.CostMicro != 12225 {
		t.Fatalf("cost = %d, want 12225", u.CostMicro)
	}
}

func TestParseUsageOpenAICachedTokens(t *testing.T) {
	body := `{"model":"gpt-4o","usage":{"prompt_tokens":2000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":1500}}}`
	u := ParseUsage(body)
	if !u.HasUsage || u.CacheRead != 1500 {
		t.Fatalf("usage = %+v", u)
	}
	// (2000-1500)*2.50 + 1500*0.25 + 100*10.00 = 2,625 micro-USD
	if u.CostMicro != 2625 {
		t.Fatalf("cost = %d, want 2625", u.CostMicro)
	}
}

// prices.yaml entries override the builtin table with an exact match, so a
// model missing from the snapshot bills correctly without a rebuild.
func TestLoadPricesOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.yaml")
	if err := os.WriteFile(path, []byte("some-new-model:\n  input: 1.00\n  output: 2.00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		priceMu.Lock()
		delete(priceOver, "some-new-model")
		priceMu.Unlock()
	})

	if _, ok := lookupPrice("some-new-model"); ok {
		t.Fatal("override visible before load")
	}
	if err := LoadPrices(path); err != nil {
		t.Fatal(err)
	}
	p, ok := lookupPrice("some-new-model")
	if !ok {
		t.Fatal("override missing after load")
	}
	if p.in != 1.00 || p.out != 2.00 {
		t.Fatalf("override = %+v", p)
	}
	// Defaults applied: 10% / 125% of input.
	if p.cacheRead != 0.10 || p.cacheWrite != 1.25 {
		t.Fatalf("cache defaults = %+v", p)
	}

	u := ParseUsage(`{"model":"some-new-model","usage":{"input_tokens":1000000,"output_tokens":1000000}}`)
	if u.CostMicro != 3000000 { // 1.00 + 2.00 USD
		t.Fatalf("cost = %d, want 3000000", u.CostMicro)
	}
}

func TestLoadPricesAbsentFile(t *testing.T) {
	if err := LoadPrices(filepath.Join(t.TempDir(), "missing.yaml")); err != nil {
		t.Fatalf("missing prices file must be a no-op, got %v", err)
	}
}
