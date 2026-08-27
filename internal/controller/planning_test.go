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
