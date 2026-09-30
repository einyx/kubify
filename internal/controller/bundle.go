package controller

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

const bundleChartsLayer = "charts.tgz"

type bundleImage struct {
	Name string
	Repo string
	Tag  string
}

func (r *StackReconciler) chartsFromBundle(ctx context.Context, stack *platformv1alpha1.Stack) (map[string]*chart.Chart, map[string]bundleImage, error) {
	ref, err := parseOCI(stack.Spec.Bundle.URL)
	if err != nil {
		return nil, nil, err
	}
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, nil, err
	}
	repo.Client = &auth.Client{
		Client:     auth.DefaultClient.Client,
		Cache:      auth.NewCache(),
		Credential: r.bundleCredential(ctx, stack, ref.Registry),
	}
	desc, err := repo.Resolve(ctx, ref.Reference)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve bundle %s: %w", stack.Spec.Bundle.URL, err)
	}
	rc, err := repo.Fetch(ctx, desc)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch bundle manifest: %w", err)
	}
	defer rc.Close()
	var manifest ocispec.Manifest
	if err := json.NewDecoder(rc).Decode(&manifest); err != nil {
		return nil, nil, fmt.Errorf("decode bundle manifest: %w", err)
	}
	images := map[string]bundleImage{}
	var layer *ocispec.Descriptor
	for i := range manifest.Layers {
		title := manifest.Layers[i].Annotations[ocispec.AnnotationTitle]
		if title == bundleChartsLayer {
			layer = &manifest.Layers[i]
		}
		if title == "bundle.json" {
			parsed, err := readBundleImages(ctx, repo, manifest.Layers[i])
			if err != nil {
				return nil, nil, err
			}
			images = parsed
		}
	}
	if layer == nil {
		return nil, nil, fmt.Errorf("bundle %s has no %s layer", stack.Spec.Bundle.URL, bundleChartsLayer)
	}
	blob, err := repo.Fetch(ctx, *layer)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", bundleChartsLayer, err)
	}
	defer blob.Close()
	dir, err := os.MkdirTemp("", "kubo-bundle-*")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	if err := untarGz(blob, dir); err != nil {
		return nil, nil, err
	}
	charts, err := loadCharts(dir)
	return charts, images, err
}

func readBundleImages(ctx context.Context, repo *remote.Repository, layer ocispec.Descriptor) (map[string]bundleImage, error) {
	rc, err := repo.Fetch(ctx, layer)
	if err != nil {
		return nil, fmt.Errorf("fetch bundle.json: %w", err)
	}
	defer rc.Close()
	var doc struct {
		Images []struct {
			Ref string `json:"ref"`
		} `json:"images"`
	}
	if err := json.NewDecoder(rc).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode bundle.json: %w", err)
	}
	out := map[string]bundleImage{}
	for _, img := range doc.Images {
		ref := strings.TrimPrefix(img.Ref, "oci://")
		name, repoName, tag := splitImage(ref)
		if name == "" {
			continue
		}
		out[name] = bundleImage{Name: name, Repo: repoName, Tag: tag}
	}
	return out, nil
}

func splitImage(ref string) (name, repo, tag string) {
	tag = "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	repo = ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	return name, repo, tag
}

func matchBundleImage(component string, images map[string]bundleImage) (bundleImage, bool) {
	if img, ok := aliasedImage(component); ok {
		return img, true
	}
	if img, ok := images[component]; ok {
		return img, true
	}
	if img, ok := images["product-"+component]; ok {
		return img, true
	}
	for name, img := range images {
		if strings.HasSuffix(name, "-"+component) {
			return img, true
		}
	}
	return bundleImage{}, false
}

func rewriteBundleValues(values map[string]interface{}, images map[string]bundleImage, pullSecret string) {
	if values == nil {
		return
	}
	disableEmptySecrets(values)
	forceOff(values, "autoscaling", "pdb", "vpa", "serviceMonitor", "externalSecret")
	rewriteImageRefs(values, images)
	if pullSecret != "" {
		values["imagePullSecrets"] = []interface{}{map[string]interface{}{"name": pullSecret}}
	}
}

func forceOff(values map[string]interface{}, keys ...string) {
	for _, key := range keys {
		child, _ := values[key].(map[string]interface{})
		if child == nil {
			child = map[string]interface{}{}
		}
		child["enabled"] = false
		values[key] = child
	}
}

func disableEmptySecrets(v interface{}) {
	m, ok := v.(map[string]interface{})
	if !ok {
		return
	}
	if en, ok := m["enabled"].(bool); ok && en {
		if sec, ok := m["existingSecret"].(string); ok && sec == "" {
			m["enabled"] = false
		}
	}
	for _, child := range m {
		disableEmptySecrets(child)
	}
}

func rewriteImageRefs(v interface{}, images map[string]bundleImage) {
	switch t := v.(type) {
	case map[string]interface{}:
		if repo, ok := t["repository"].(string); ok {
			if img, ok := lookupBundleImage(repo, images); ok {
				t["repository"] = img.Repo
				if tag, _ := t["tag"].(string); tag == "" || tag == "latest" {
					t["tag"] = img.Tag
				}
			}
		}
		for _, child := range t {
			rewriteImageRefs(child, images)
		}
	case []interface{}:
		for _, child := range t {
			rewriteImageRefs(child, images)
		}
	}
}

func lookupBundleImage(repo string, images map[string]bundleImage) (bundleImage, bool) {
	name := repo
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		name = repo[i+1:]
	}
	if img, ok := aliasedImage(name); ok {
		return img, true
	}
	name = strings.TrimPrefix(name, "product-")
	if img, ok := images[name]; ok {
		return img, true
	}
	if img, ok := images["product-"+name]; ok {
		return img, true
	}
	for n, img := range images {
		if n == name || strings.HasSuffix(n, "-"+name) || strings.TrimPrefix(n, "product-") == name {
			return img, true
		}
	}
	return bundleImage{}, false
}

func aliasedImage(name string) (bundleImage, bool) {
	aliases := map[string]string{
		"s3proxy":        "product-storage-engine",
		"storage-engine": "product-storage-engine",
	}
	target, ok := aliases[name]
	if !ok {
		return bundleImage{}, false
	}
	return bundleImage{Name: target, Repo: "ghcr.io/einyx/" + target, Tag: "latest"}, true
}

func applyBundleImage(values map[string]interface{}, img bundleImage, pullSecret string) {
	image, _ := values["image"].(map[string]interface{})
	if image == nil {
		image = map[string]interface{}{}
	}
	image["repository"] = img.Repo
	image["tag"] = img.Tag
	image["pullPolicy"] = "IfNotPresent"
	values["image"] = image
	if pullSecret == "" {
		return
	}
	values["imagePullSecrets"] = []interface{}{map[string]interface{}{"name": pullSecret}}
}

func (r *StackReconciler) bundleCredential(ctx context.Context, stack *platformv1alpha1.Stack, registryHost string) auth.CredentialFunc {
	return func(context.Context, string) (auth.Credential, error) {
		if stack.Spec.Bundle.SecretRef == nil || stack.Spec.Bundle.SecretRef.Name == "" {
			return auth.EmptyCredential, nil
		}
		var secret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: stack.Spec.Bundle.SecretRef.Name}, &secret); err != nil {
			return auth.EmptyCredential, err
		}
		raw := secret.Data[".dockerconfigjson"]
		if len(raw) == 0 {
			raw = secret.Data["config.json"]
		}
		var cfg struct {
			Auths map[string]struct {
				Auth     string `json:"auth"`
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auths"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return auth.EmptyCredential, fmt.Errorf("parse pull secret: %w", err)
		}
		entry, ok := cfg.Auths[registryHost]
		if !ok {
			for host, e := range cfg.Auths {
				if strings.Contains(host, registryHost) {
					entry, ok = e, true
					break
				}
			}
		}
		if !ok {
			return auth.EmptyCredential, fmt.Errorf("pull secret has no credentials for %s", registryHost)
		}
		user, pass := entry.Username, entry.Password
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return auth.EmptyCredential, err
			}
			user, pass, _ = strings.Cut(string(decoded), ":")
		}
		return auth.Credential{Username: user, Password: pass}, nil
	}
}

func parseOCI(raw string) (registry.Reference, error) {
	raw = strings.TrimPrefix(raw, "oci://")
	ref, err := registry.ParseReference(raw)
	if err != nil {
		return registry.Reference{}, fmt.Errorf("parse bundle url %q: %w", raw, err)
	}
	if ref.Reference == "" {
		ref.Reference = "latest"
	}
	return ref, nil
}

func untarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("open charts.tgz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
			return fmt.Errorf("illegal path in charts archive: %s", hdr.Name)
		}
		base := filepath.Base(hdr.Name)
		if strings.HasPrefix(base, "._") || strings.Contains(hdr.Name, "__MACOSX") {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}

func loadCharts(root string) (map[string]*chart.Chart, error) {
	out := map[string]*chart.Chart{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "Chart.yaml" {
			return err
		}
		if strings.Contains(filepath.ToSlash(filepath.Dir(path)), "/charts/") {
			return nil
		}
		ch, err := loader.Load(filepath.Dir(path))
		if err != nil {
			return err
		}
		name := filepath.Base(filepath.Dir(path))
		if ch.Name() != "" {
			out[ch.Name()] = ch
		}
		out[name] = ch
		return nil
	})
	return out, err
}
