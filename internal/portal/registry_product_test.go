package portal

import (
	"context"
	"strings"
	"testing"
)

func TestProductTemplateInRegistry(t *testing.T) {
	r := NewRegistry(defaultTemplatesDir, nil)
	tpl, err := r.Get(context.Background(), "product")
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Meta.ID != "product" || !strings.Contains(tpl.Body, "{{.Tenant}}.kubify.foundation") {
		t.Fatalf("template: %+v", tpl.Meta)
	}
}
