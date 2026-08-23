package controller

import (
	"encoding/json"
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
		{Name: "backend", DependsOnReady: []string{"postgres"}},
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



func TestExtractImages(t *testing.T) {
	m := `---
kind: Deployment
spec:
  template:
    spec:
      containers:
      - image: ghcr.io/einyx/backend:v0.5.62
      - image: "kubifyregistry.azurecr.io/cached/ecr-kubify/opa:v1.18.1"
---
kind: ConfigMap
data:
  # not a container image, must be ignored by kind filter? (we match all image: lines)
  image: not/an/image
`
	imgs := extractImages(m)
	if len(imgs) != 3 {
		t.Fatalf("expected 3 images, got %d: %+v", len(imgs), imgs)
	}
	if imgs[0].Repository != "ghcr.io/einyx/backend" || imgs[0].Tag != "v0.5.62" {
		t.Errorf("bad split: %+v", imgs[0])
	}
}
