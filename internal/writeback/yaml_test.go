package writeback

import (
	"strings"
	"testing"
)

func TestMutateFeatureFlagsPreservesComments(t *testing.T) {
	src := []byte("# file comment\napiVersion: platform.kubo.io/v1alpha1\nkind: Stack\nmetadata:\n  name: product\n  namespace: integration\nspec:\n  # keep me\n  featureFlags:\n    old: \"false\"\n")
	out, err := MutateFeatureFlags(src, "integration", "product", map[string]string{"old": "false"}, map[string]string{"old": "true", "new": "false"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# file comment", "# keep me", "old: 'true'", "new: 'false'"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
}

func TestMutateFeatureFlagsDetectsConflict(t *testing.T) {
	src := []byte("apiVersion: platform.kubo.io/v1alpha1\nkind: Stack\nmetadata: {name: x, namespace: ns}\nspec: {featureFlags: {a: \"true\"}}\n")
	if _, err := MutateFeatureFlags(src, "ns", "x", map[string]string{"a": "false"}, map[string]string{"a": "true"}); err == nil {
		t.Fatal("expected conflict")
	}
}
