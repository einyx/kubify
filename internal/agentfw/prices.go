package agentfw

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"sigs.k8s.io/yaml"
)

// Cache-rate defaults when a price entry does not set them explicitly:
// Anthropic prompt caching charges 10% of the input rate for reads and
// 125% for writes; OpenAI and Google cached reads land in the same range.
const (
	defaultCacheReadMult  = 0.10
	defaultCacheWriteMult = 1.25
)

// withDefaults fills unset cache rates with the provider-standard
// multiples of the input rate.
func (p price) withDefaults() price {
	if p.cacheRead == 0 {
		p.cacheRead = p.in * defaultCacheReadMult
	}
	if p.cacheWrite == 0 {
		p.cacheWrite = p.in * defaultCacheWriteMult
	}
	return p
}

var (
	priceMu   sync.RWMutex
	priceOver = map[string]price{} // file overrides, merged over the builtin table

	// unpricedLogged dedupes the "model has no price" warning: one line
	// per model per process, not one per request.
	unpricedLogged sync.Map
)

// priceFileEntry is one model's row in prices.yaml.
type priceFileEntry struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// LoadPrices merges a prices YAML file (map of model → rates, USD per
// million tokens) over the builtin snapshot so billing survives model
// releases without a rebuild:
//
//	claude-sonnet-4-6:
//	  input: 3.00
//	  output: 15.00
//	  cacheRead: 0.30    # optional — defaults to 10% of input
//	  cacheWrite: 3.75   # optional — defaults to 125% of input
//
// Keys are matched exactly (after lowercasing and Bedrock-ARN trimming);
// prefix matching stays builtin-table only.
func LoadPrices(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // no prices file is fine — builtin snapshot applies
	}
	if err != nil {
		return err
	}
	var m map[string]priceFileEntry
	if err := yaml.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("agentfw prices: %w", err)
	}
	priceMu.Lock()
	for k, e := range m {
		priceOver[strings.ToLower(k)] = price{in: e.Input, out: e.Output, cacheRead: e.CacheRead, cacheWrite: e.CacheWrite}
	}
	priceMu.Unlock()
	return nil
}

// warnUnpriced logs (once per model) models that report usage but carry no
// price entry — without this they silently bill at $0.
func warnUnpriced(model string) {
	if model == "" {
		return
	}
	if _, seen := unpricedLogged.LoadOrStore(model, struct{}{}); !seen {
		log.Printf("agentfw prices: model %q has no price entry — usage reported with cost 0; add it to prices.yaml", model)
	}
}
