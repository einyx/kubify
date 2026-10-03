package controller

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRedactValues(t *testing.T) {
	values := map[string]interface{}{
		"replicaCount": 1,
		"config": map[string]interface{}{
			"host":     "postgres-postgresql",
			"port":     5432,
			"user":     "foundation",
			"password": "hunter2",
		},
		"configMap": map[string]interface{}{
			"data": map[string]interface{}{
				"MX_ANTHROPIC_API_KEY":   "sk-ant-123",
				"MX_ZENDESK_API_TOKEN":   "tok-456",
				"MX_POSTGRES_HOST":       "postgres-postgresql",
				"MX_AUTH0_CLIENT_SECRET": "topsecret",
			},
		},
		"secret": map[string]interface{}{
			"data": map[string]interface{}{
				"MX_API_KEY": "key-789",
			},
		},
		"image": map[string]interface{}{
			"repository": "example.com/foo",
			"tag":        "v1.0.0",
		},
		"env": []interface{}{
			map[string]interface{}{"name": "PLAIN", "value": "visible"},
			map[string]interface{}{"name": "DB_PASSWORD", "value": "hidden"},
		},
	}

	out, err := yaml.Marshal(redactValues(values))
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(out)

	for _, leaked := range []string{"hunter2", "sk-ant-123", "tok-456", "topsecret", "key-789", "hidden"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("redacted output leaked sensitive value %q:\n%s", leaked, rendered)
		}
	}
	for _, kept := range []string{"postgres-postgresql", "foundation", "example.com/foo", "v1.0.0", "visible"} {
		if !strings.Contains(rendered, kept) {
			t.Errorf("redacted output dropped non-sensitive value %q:\n%s", kept, rendered)
		}
	}
	if !strings.Contains(rendered, "[REDACTED]") {
		t.Errorf("expected [REDACTED] placeholders in output:\n%s", rendered)
	}
}

func TestRedactValuesDoesNotMutateInput(t *testing.T) {
	values := map[string]interface{}{
		"config": map[string]interface{}{
			"password": "hunter2",
		},
	}
	redactValues(values)
	if values["config"].(map[string]interface{})["password"] != "hunter2" {
		t.Fatal("redactValues mutated its input")
	}
}
