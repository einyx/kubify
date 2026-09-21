package portal

import (
	"context"
	"strings"
	"testing"
)

func TestProductTemplateInRegistry(t *testing.T) {
	r := NewRegistry(defaultTemplatesDir, nil)
	// The builtin must mirror the canonical configmap template (same id, so
	// the registry's id-precedence collapses them — one "Product (full)").
	tpl, err := r.Get(context.Background(), "full")
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Meta.ID != "full" || !strings.Contains(tpl.Body, "{{.Tenant}}.kubify.foundation") {
		t.Fatalf("template: %+v", tpl.Meta)
	}
	raw, err := builtinFS.ReadFile("templates/full.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "seedVault") {
		t.Fatal("builtin full template lost the seedVault block — it must stay a copy of the configmap product-full")
	}
	if _, err := r.Get(context.Background(), "product"); err == nil {
		t.Fatal("legacy 'product' id must be gone (one template, one id)")
	}
}
