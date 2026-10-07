package portal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func TestProductTemplateInRegistry(t *testing.T) {
	r := NewRegistry(defaultTemplatesDir, nil)
	// The builtin must mirror the canonical configmap template (same id, so
	// the registry's id-precedence collapses them — one "Product (full)").
	tpl, err := r.Get(context.Background(), "full")
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Meta.ID != "full" || !strings.Contains(tpl.Body, "{{.Tenant}}.meshx.foundation") {
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
	objects, err := renderTemplate(tpl.Body, "integration")
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range objects {
		if stack, ok := obj.(*platformv1alpha1.Stack); ok {
			if stack.Namespace != "integration" {
				t.Fatalf("unexpected namespace: %s", stack.Namespace)
			}
			if stack.Spec.SeedVault == nil || len(stack.Spec.SeedVault.Static) == 0 || len(stack.Spec.SeedVault.Generated) == 0 {
				t.Fatal("rendered stack must include bootstrap secrets in spec.seedVault")
			}
			frontend, ok := stack.Spec.ComponentValues["frontend"]
			if !ok {
				t.Fatal("rendered stack has no frontend values")
			}
			var values struct {
				ExistingAuthSecret struct {
					Enabled bool   `json:"enabled"`
					Name    string `json:"name"`
				} `json:"existingAuthSecret"`
			}
			if err := json.Unmarshal(frontend.Raw, &values); err != nil {
				t.Fatalf("decode frontend values: %v", err)
			}
			if !values.ExistingAuthSecret.Enabled || values.ExistingAuthSecret.Name != "product-frontend-auth0" {
				t.Fatalf("frontend must consume propagated Auth0 secret: %+v", values.ExistingAuthSecret)
			}
			return
		}
	}
	t.Fatal("rendered template has no Stack")
}

func TestProductTemplatesEnableWatcherHTTPOrchestrator(t *testing.T) {
	r := NewRegistry(defaultTemplatesDir, nil)
	for _, templateID := range []string{"full", "lite"} {
		t.Run(templateID, func(t *testing.T) {
			tpl, err := r.Get(context.Background(), templateID)
			if err != nil {
				t.Fatal(err)
			}
			objects, err := renderTemplate(tpl.Body, "acme")
			if err != nil {
				t.Fatal(err)
			}
			for _, obj := range objects {
				stack, ok := obj.(*platformv1alpha1.Stack)
				if !ok {
					continue
				}
				backend := stack.Spec.ComponentValues["backend"]
				var values struct {
					Config struct {
						Orchestrator struct {
							WorkerEnabled bool `json:"worker_enabled"`
						} `json:"orchestrator"`
						Watcher struct {
							HTTP struct {
								URL string `json:"url"`
							} `json:"http"`
						} `json:"watcher"`
					} `json:"config"`
				}
				if err := json.Unmarshal(backend.Raw, &values); err != nil {
					t.Fatal(err)
				}
				if !values.Config.Orchestrator.WorkerEnabled || values.Config.Watcher.HTTP.URL != "http://watcher-acme:8000" {
					t.Fatalf("backend watcher/orchestrator values: %+v", values.Config)
				}
				return
			}
			t.Fatal("rendered template has no Stack")
		})
	}
}
