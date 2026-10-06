package controller

import (
	"testing"
)

func bi(name, repo, tag string) bundleImage {
	return bundleImage{Name: name, Repo: repo, Tag: tag}
}

func TestSplitRegistry(t *testing.T) {
	tests := []struct {
		in, reg, path string
	}{
		{"ghcr.io/example/opa", "ghcr.io", "example/opa"},
		{"myacr.azurecr.io/a/b/c", "myacr.azurecr.io", "a/b/c"},
		{"opa", "", "opa"},
	}
	for _, tt := range tests {
		reg, path := splitRegistry(tt.in)
		if reg != tt.reg || path != tt.path {
			t.Errorf("splitRegistry(%q) = (%q,%q), want (%q,%q)", tt.in, reg, path, tt.reg, tt.path)
		}
	}
}

func TestSplitImage(t *testing.T) {
	tests := []struct {
		in, name, repo, tag string
	}{
		{"ghcr.io/example/opa:v1.2", "opa", "ghcr.io/example/opa", "v1.2"},
		{"ghcr.io/example/opa", "opa", "ghcr.io/example/opa", "latest"},
		{"opa:latest", "", "opa", "latest"}, // bare ref: no "/" so name stays empty
		{"localhost:5000/opa:2", "opa", "localhost:5000/opa", "2"},
	}
	for _, tt := range tests {
		name, repo, tag := splitImage(tt.in)
		if name != tt.name || repo != tt.repo || tag != tt.tag {
			t.Errorf("splitImage(%q) = (%q,%q,%q), want (%q,%q,%q)", tt.in, name, repo, tag, tt.name, tt.repo, tt.tag)
		}
	}
}

func TestMatchBundleImage(t *testing.T) {
	images := map[string]bundleImage{
		"backend":        bi("backend", "reg.io/example/backend", "v1"),
		"acme-trino":     bi("acme-trino", "reg.io/example/acme-trino", "v2"),
		"storage-engine": bi("storage-engine", "reg.io/example/storage-engine", "v3"),
	}
	tests := []struct {
		component string
		wantTag   string
		wantOK    bool
	}{
		{"backend", "v1", true},        // exact
		{"trino", "v2", true},          // product-prefixed bundle name
		{"storage-engine", "v3", true}, // exact
		{"missing", "", false},
	}
	for _, tt := range tests {
		img, ok := matchBundleImage(tt.component, images)
		if ok != tt.wantOK {
			t.Errorf("matchBundleImage(%q) ok = %v, want %v", tt.component, ok, tt.wantOK)
			continue
		}
		if ok && img.Tag != tt.wantTag {
			t.Errorf("matchBundleImage(%q) tag = %q, want %q", tt.component, img.Tag, tt.wantTag)
		}
	}
	// A product-prefixed component must NOT fall through to another
	// product's image by basename — dai-backend is not foundation/backend.
	if _, ok := matchBundleImage("dai-backend", images); ok {
		t.Error("dai-backend must not match the foundation backend image")
	}
}


func TestRewriteBundleValues(t *testing.T) {
	values := map[string]interface{}{
		"autoscaling": map[string]interface{}{"enabled": true},
		"postgres": map[string]interface{}{
			"enabled":        true,
			"existingSecret": "", // empty -> disabled
		},
		"image": map[string]interface{}{
			"repository": "ghcr.io/example/backend",
			"tag":        "latest",
		},
	}
	images := map[string]bundleImage{
		"backend": bi("backend", "mirror.io/example/backend", "v7"),
	}
	rewriteBundleValues(values, images, "pull-secret")

	if as := values["autoscaling"].(map[string]interface{}); as["enabled"] != false {
		t.Error("autoscaling not forced off")
	}
	if pg := values["postgres"].(map[string]interface{}); pg["enabled"] != false {
		t.Error("empty existingSecret should disable the component")
	}
	// Image refs are deliberately NOT rewritten here — the
	// component-scoped applyBundleImage owns that (see
	// TestRewriteBundleValuesDoesNotTouchImages).
	img := values["image"].(map[string]interface{})
	if img["repository"] != "ghcr.io/example/backend" {
		t.Errorf("repository must be untouched: %v", img["repository"])
	}
	if img["tag"] != "latest" {
		t.Errorf("tag must be untouched: %v", img["tag"])
	}
	secrets := values["imagePullSecrets"].([]interface{})
	if len(secrets) != 1 || secrets[0].(map[string]interface{})["name"] != "pull-secret" {
		t.Errorf("imagePullSecrets wrong: %v", secrets)
	}

	// An explicit non-latest tag must be preserved.
	values["image"].(map[string]interface{})["tag"] = "v9-custom"
	rewriteBundleValues(values, images, "")
	if values["image"].(map[string]interface{})["tag"] != "v9-custom" {
		t.Error("explicit tag was overwritten")
	}
}

func TestRewriteBundleValuesDoesNotTouchImages(t *testing.T) {
	// The recursive repo rewrite was removed: rewriteBundleValues must
	// never touch image repositories (the component-scoped
	// matchBundleImage + applyBundleImage pair owns that), otherwise one
	// product's bundle rewrites another product's repos by basename.
	values := map[string]interface{}{
		"image": map[string]interface{}{
			"registry":   "old.reg.io",
			"repository": "example/dai/backend",
			"tag":        "v0.2.79",
		},
	}
	images := map[string]bundleImage{
		"backend": bi("backend", "mirror.io/example/backend", "v7"),
	}
	rewriteBundleValues(values, images, "")
	img := values["image"].(map[string]interface{})
	if img["repository"] != "example/dai/backend" || img["tag"] != "v0.2.79" {
		t.Errorf("image rewritten: %v", img)
	}
}

func TestApplyBundleImage(t *testing.T) {
	values := map[string]interface{}{}
	img := bi("backend", "mirror.io/example/backend", "v1")
	applyBundleImage(values, img, "acr-pull")

	got := values["image"].(map[string]interface{})
	if got["repository"] != "mirror.io/example/backend" || got["tag"] != "v1" || got["pullPolicy"] != "IfNotPresent" {
		t.Errorf("applyBundleImage wrong: %v", got)
	}
	if _, ok := values["imagePullSecrets"]; !ok {
		t.Error("pull secret not applied")
	}
}

func TestReadBundleImagesParsesRefs(t *testing.T) {
	// readBundleImages strips the oci:// prefix before splitImage; cover the
	// resulting shapes it must tolerate.
	name, repo, tag := splitImage("reg.io/example/opa")
	if name != "opa" || repo != "reg.io/example/opa" {
		t.Errorf("split wrong: %q %q", name, repo)
	}
	if tag != "latest" {
		t.Errorf("default tag: %q", tag)
	}
}

func TestParseOCI(t *testing.T) {
	if _, err := parseOCI("::::not-a-ref"); err == nil {
		t.Error("expected error for invalid bundle URL")
	}
	ref, err := parseOCI("oci://ghcr.io/org/product-bundle:0.0.14")
	if err != nil {
		t.Fatalf("parseOCI: %v", err)
	}
	if ref.Registry != "ghcr.io" || ref.Repository != "org/product-bundle" || ref.Reference != "0.0.14" {
		t.Errorf("parseOCI wrong: %+v", ref)
	}
	// Missing tag defaults to latest.
	ref, err = parseOCI("ghcr.io/org/product-bundle")
	if err != nil {
		t.Fatalf("parseOCI no tag: %v", err)
	}
	if ref.Reference != "latest" {
		t.Errorf("default reference = %q, want latest", ref.Reference)
	}
}
