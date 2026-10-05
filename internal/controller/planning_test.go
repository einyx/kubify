package controller

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

type apiextensionsJSON = map[string]apiextensionsv1.JSON

func mustJSON(t *testing.T, s string) *apiextensionsv1.JSON {
	t.Helper()
	return &apiextensionsv1.JSON{Raw: json.RawMessage(s)}
}

func TestTopoOrderRespectsDependencies(t *testing.T) {
	comps := []platformv1alpha1.StackComponentSpec{
		{Name: "frontend", DependsOn: []string{"backend"}},
		{Name: "backend", DependsOn: []string{"postgres"}},
		{Name: "postgres"},
	}
	got, err := topoOrder(comps)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, n := range got {
		pos[n] = i
	}
	for _, want := range []struct{ before, after string }{
		{"postgres", "backend"}, {"backend", "frontend"},
	} {
		if pos[want.before] >= pos[want.after] {
			t.Errorf("%s should come before %s, got %v", want.before, want.after, got)
		}
	}
}

func TestTopoOrderDetectsCycle(t *testing.T) {
	comps := []platformv1alpha1.StackComponentSpec{
		{Name: "a", DependsOn: []string{"b"}},
		{Name: "b", DependsOn: []string{"a"}},
	}
	if _, err := topoOrder(comps); err == nil {
		t.Error("expected cycle error")
	}
}

func TestTopoOrderUnknownDep(t *testing.T) {
	comps := []platformv1alpha1.StackComponentSpec{{Name: "a", DependsOn: []string{"nope"}}}
	if _, err := topoOrder(comps); err == nil {
		t.Error("expected unknown dep error")
	}
}

func TestMergeValues(t *testing.T) {
	dst := map[string]interface{}{"a": 1, "nested": map[string]interface{}{"x": 1, "y": 2}}
	src := map[string]interface{}{"b": 2, "nested": map[string]interface{}{"y": 3, "z": 4}}
	got := mergeValues(dst, src)
	if got["a"] != 1 || got["b"] != 2 {
		t.Errorf("unexpected merge: %v", got)
	}
	n := got["nested"].(map[string]interface{})
	if n["x"] != 1 || n["y"] != 3 || n["z"] != 4 {
		t.Errorf("unexpected deep merge: %v", n)
	}
}

func TestResolveComponentValuesPrecedence(t *testing.T) {
	def := mustJSON(t, `{"replicas": 1, "image": {"tag": "v1"}}`)
	stack := mustJSON(t, `{"components": {"backend": {"replicas": 3}}}`)
	comp := mustJSON(t, `{"image": {"tag": "v2"}}`)
	got := resolveComponentValues(def, stack, map[string]apiextensionsv1.JSON{"backend": *comp}, "backend")
	if got["replicas"] != float64(3) {
		t.Errorf("stack override lost: %v", got)
	}
	img := got["image"].(map[string]interface{})
	if img["tag"] != "v2" {
		t.Errorf("component override lost: %v", img)
	}
}

func TestGenerateValueKinds(t *testing.T) {
	hexv, err := generateValue(platformv1alpha1.GeneratedKey{Kind: "hex", Length: 16})
	if err != nil || len(hexv.data) != 16 {
		t.Fatalf("hex: %q %v", hexv.data, err)
	}
	if _, err := hex.DecodeString(hexv.data); err != nil {
		t.Errorf("hex not hex: %v", err)
	}
	b64, err := generateValue(platformv1alpha1.GeneratedKey{Kind: "base64", Length: 32})
	if err != nil || len(b64.data) < 40 {
		t.Fatalf("base64: %v", err)
	}
	u, err := generateValue(platformv1alpha1.GeneratedKey{Kind: "uuid"})
	if err != nil || len(u.data) != 36 || u.data[14] != '4' {
		t.Fatalf("uuid: %q %v", u.data, err)
	}
	bc, err := generateValue(platformv1alpha1.GeneratedKey{Kind: "bcrypt"})
	if err != nil || len(bc.data) < 59 || bc.data[:4] != "$2a$" && bc.data[:4] != "$2b$" && bc.data[:4] != "$2y$" {
		t.Fatalf("bcrypt hash: %q %v", bc.data, err)
	}
	tls, err := generateValue(platformv1alpha1.GeneratedKey{Kind: "tls"})
	if err != nil || !strings.Contains(tls.cert, "BEGIN CERTIFICATE") || !strings.Contains(tls.key, "PRIVATE KEY") {
		t.Fatalf("tls: %v", err)
	}
	if _, err := generateValue(platformv1alpha1.GeneratedKey{Kind: "nope"}); err == nil {
		t.Error("unknown kind should error")
	}
}

func TestDefaultFrontendBaseURL(t *testing.T) {
	t.Run("derives from VS host with placeholder", func(t *testing.T) {
		v := map[string]interface{}{
			"virtualService": map[string]interface{}{"host": "{{ namespace }}.meshx.foundation"},
			"env":            map[string]interface{}{"auth0": map[string]interface{}{"enabled": "true"}},
		}
		defaultFrontendBaseURL("acme", v)
		got := v["env"].(map[string]interface{})["auth0"].(map[string]interface{})["baseurl"]
		if got != "https://acme.meshx.foundation/" {
			t.Fatalf("baseurl = %v", got)
		}
	})
	t.Run("explicit value wins", func(t *testing.T) {
		v := map[string]interface{}{
			"virtualService": map[string]interface{}{"host": "acme.meshx.foundation"},
			"env":            map[string]interface{}{"auth0": map[string]interface{}{"baseurl": "https://custom.example.com/"}},
		}
		defaultFrontendBaseURL("acme", v)
		got := v["env"].(map[string]interface{})["auth0"].(map[string]interface{})["baseurl"]
		if got != "https://custom.example.com/" {
			t.Fatalf("baseurl = %v", got)
		}
	})
	t.Run("no host, no env map yet", func(t *testing.T) {
		v := map[string]interface{}{"virtualService": map[string]interface{}{"host": "{{ namespace }}.demo.meshx.foundation"}}
		defaultFrontendBaseURL("product-z", v)
		got := v["env"].(map[string]interface{})["auth0"].(map[string]interface{})["baseurl"]
		if got != "https://product-z.demo.meshx.foundation/" {
			t.Fatalf("baseurl = %v", got)
		}
	})
	t.Run("missing host is a no-op", func(t *testing.T) {
		v := map[string]interface{}{}
		defaultFrontendBaseURL("acme", v)
		if e, ok := v["env"]; ok {
			t.Fatalf("env should stay absent, got %v", e)
		}
	})
}

func TestApplyImageTags(t *testing.T) {
	t.Run("overrides matching component tag", func(t *testing.T) {
		v := map[string]interface{}{"image": map[string]interface{}{"repository": "reg/backend", "tag": "v0.2.78"}}
		applyImageTags(map[string]string{"backend": "v0.2.79"}, "backend", v)
		img := v["image"].(map[string]interface{})
		if img["tag"] != "v0.2.79" || img["repository"] != "reg/backend" {
			t.Fatalf("image = %v", img)
		}
	})
	t.Run("creates image map when absent", func(t *testing.T) {
		v := map[string]interface{}{}
		applyImageTags(map[string]string{"ai": "dev-42"}, "ai", v)
		img := v["image"].(map[string]interface{})
		if img["tag"] != "dev-42" {
			t.Fatalf("tag = %v", img["tag"])
		}
	})
	t.Run("components not in the map untouched", func(t *testing.T) {
		v := map[string]interface{}{}
		applyImageTags(map[string]string{"backend": "v1"}, "frontend", v)
		if len(v) != 0 {
			t.Fatalf("unrelated component modified: %v", v)
		}
	})
	t.Run("empty tag value ignored", func(t *testing.T) {
		v := map[string]interface{}{}
		applyImageTags(map[string]string{"backend": ""}, "backend", v)
		if len(v) != 0 {
			t.Fatalf("empty tag should be a no-op: %v", v)
		}
	})
	t.Run("nil map no-op", func(t *testing.T) {
		v := map[string]interface{}{}
		applyImageTags(nil, "backend", v)
		if len(v) != 0 {
			t.Fatal("nil imageTags should not modify values")
		}
	})
}

func TestApplyFeatureFlags(t *testing.T) {
	t.Run("merges normalized flags into frontend env", func(t *testing.T) {
		v := map[string]interface{}{
			"env": map[string]interface{}{
				"feature_flags": map[string]interface{}{"connectors_enabled": "false"},
			},
		}
		applyFeatureFlags(map[string]string{
			"MX_FF_CONNECTORS_ENABLED":   "true",
			"query_exports_enabled":      "true",
			"MX_FF_LANDSCAPE_AUTO_STACK": "true",
		}, "frontend", v)
		ff := v["env"].(map[string]interface{})["feature_flags"].(map[string]interface{})
		for k, want := range map[string]string{
			"connectors_enabled":    "true",
			"query_exports_enabled": "true",
			"landscape_auto_stack":  "true",
		} {
			if got := ff[k]; got != want {
				t.Fatalf("flag %s = %v, want %v", k, got, want)
			}
		}
	})
	t.Run("creates env structure when absent", func(t *testing.T) {
		v := map[string]interface{}{}
		applyFeatureFlags(map[string]string{"connectors_enabled": "true"}, "frontend", v)
		got := v["env"].(map[string]interface{})["feature_flags"].(map[string]interface{})["connectors_enabled"]
		if got != "true" {
			t.Fatalf("connectors_enabled = %v", got)
		}
	})
	t.Run("non-frontend components untouched", func(t *testing.T) {
		v := map[string]interface{}{}
		applyFeatureFlags(map[string]string{"connectors_enabled": "true"}, "backend", v)
		if _, ok := v["env"]; ok {
			t.Fatal("backend values should not gain env")
		}
	})
	t.Run("empty flags no-op", func(t *testing.T) {
		v := map[string]interface{}{}
		applyFeatureFlags(nil, "frontend", v)
		if len(v) != 0 {
			t.Fatal("nil flags should not modify values")
		}
	})
}
