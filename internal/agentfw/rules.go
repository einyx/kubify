package agentfw

import (
	"fmt"
	"os"
	"regexp"

	"sigs.k8s.io/yaml"
)

type ruleBundle struct {
	DLP       []ruleEntry `json:"dlp"`
	Injection []ruleEntry `json:"injection"`
}

type ruleEntry struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// LoadRules appends patterns from a community rules YAML file to the global
// dlpPatterns and injectionPatterns slices.
func LoadRules(path string) error {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil // no rules file is fine
	}
	if err != nil {
		return err
	}
	var bundle ruleBundle
	if err := yaml.Unmarshal(b, &bundle); err != nil {
		return fmt.Errorf("agentfw rules: %w", err)
	}
	for _, r := range bundle.DLP {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("agentfw rules: dlp pattern %q: %w", r.Name, err)
		}
		dlpPatterns = append(dlpPatterns, dlpPattern{name: r.Name, re: re})
	}
	for _, r := range bundle.Injection {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("agentfw rules: injection pattern %q: %w", r.Name, err)
		}
		injectionPatterns = append(injectionPatterns, dlpPattern{name: r.Name, re: re})
	}
	return nil
}
