// Package main builds and pushes a product bundle (charts.tgz + bundle.json)
// as an OCI artifact. Usage: kubo-bundle --push <oci-ref> <chart-dir> [<chart-dir> ...]
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	godigest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart/loader"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	sigsyaml "sigs.k8s.io/yaml"
)

func main() {
	push := flag.String("push", "", "OCI reference to push, e.g. ghcr.io/org/product-bundle:0.0.9")
	outDir := flag.String("out", "", "write charts.tgz + bundle.json to this directory instead of pushing")
	imagesFile := flag.String("images", "", "YAML/JSON file of curated image refs: {images: [{ref: ghcr.io/org/foo:1.0}, ...]}. Overrides chart-default scan.")
	flag.Parse()
	if (*push == "" && *outDir == "") || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: kubo-bundle (--push <ref> | --out <dir>) <chart-dir> [<chart-dir> ...]")
		os.Exit(2)
	}
	chartsTGZ, images, err := build(flag.Args())
	if err != nil {
		log.Fatal(err)
	}
	if *imagesFile != "" {
		images, err = loadImages(*imagesFile)
		if err != nil {
			log.Fatal(err)
		}
	}
	bundleJSON, _ := json.MarshalIndent(map[string]any{"images": images}, "", "  ")
	if *outDir != "" {
		if err := os.MkdirAll(*outDir, 0755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*outDir, "charts.tgz"), chartsTGZ, 0644); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*outDir, "bundle.json"), bundleJSON, 0644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s (%d charts, %d images)\n", *outDir, len(flag.Args()), len(images))
		return
	}
	if err := pushOCI(context.Background(), *push, chartsTGZ, bundleJSON); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("pushed %s (%d charts, %d images)\n", *push, len(flag.Args()), len(images))
}

type imageRef struct {
	Ref string `json:"ref" yaml:"ref"`
}

func loadImages(path string) ([]imageRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Images []imageRef `json:"images" yaml:"images"`
	}
	if err := sigsyaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Images, nil
}

func build(dirs []string) ([]byte, []imageRef, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	var images []imageRef
	seen := map[string]bool{}

	for _, dir := range dirs {
		ch, err := loader.LoadDir(dir)
		if err != nil {
			return nil, nil, fmt.Errorf("load %s: %w", dir, err)
		}
		// Copy the chart directory into the outer tar under its chart name so the
		// controller's loadCharts walk finds Chart.yaml.
		if err := addDir(tw, dir, ch.Name()); err != nil {
			return nil, nil, fmt.Errorf("tar %s: %w", dir, err)
		}
		for _, ref := range extractImages(ch.Values) {
			if seen[ref] {
				continue
			}
			seen[ref] = true
			images = append(images, imageRef{Ref: ref})
		}
	}
	if err := tw.Close(); err != nil {
		return nil, nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), images, nil
}

func addDir(tw *tar.Writer, src, name string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdrName := filepath.ToSlash(filepath.Join(name, rel))
		if info.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: hdrName + "/", Mode: 0755, Typeflag: tar.TypeDir})
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: hdrName, Mode: 0644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
}

// extractImages walks the chart's default Values looking for image maps with a
// non-empty, non-templated repository, and emits "<repo>:<tag>" refs.
func extractImages(v any) []string {
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case map[string]any:
			if repo, ok := t["repository"].(string); ok && repo != "" && !strings.Contains(repo, "{{") {
				tag, _ := t["tag"].(string)
				if tag == "" {
					tag = "latest"
				}
				out = append(out, repo+":"+tag)
			}
			for _, c := range t {
				walk(c)
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return out
}

func pushOCI(ctx context.Context, ref string, chartsTGZ, bundleJSON []byte) error {
	store := memory.New()

	chartsDesc, err := addBlob(ctx, store, "charts.tgz", "application/vnd.kubo.bundle.charts.v1.tar+gzip", chartsTGZ)
	if err != nil {
		return err
	}
	bundleDesc, err := addBlob(ctx, store, "bundle.json", "application/vnd.kubo.bundle.images.v1+json", bundleJSON)
	if err != nil {
		return err
	}
	configDesc, err := addBlob(ctx, store, "", "application/vnd.kubo.bundle.config.v1+json", []byte("{}"))
	if err != nil {
		return err
	}

	manifest := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      configDesc,
		Layers:      []ocispec.Descriptor{chartsDesc, bundleDesc},
		Annotations: map[string]string{"org.opencontainers.image.title": "product-bundle"},
	}
	manifestJSON, _ := json.Marshal(manifest)
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    godigest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	if err := store.Push(ctx, manifestDesc, bytes.NewReader(manifestJSON)); err != nil {
		return err
	}

	parts := strings.SplitN(ref, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("ref must be <repo>:<tag>, got %s", ref)
	}
	repo, err := remote.NewRepository(parts[0])
	if err != nil {
		return err
	}
	if cs, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		repo.Client = &auth.Client{
			Client:     auth.DefaultClient.Client,
			Cache:      auth.NewCache(),
			Credential: credentials.Credential(cs),
		}
	}
	if err := store.Tag(ctx, manifestDesc, parts[1]); err != nil {
		return err
	}
	_, err = oras.Copy(ctx, store, parts[1], repo, parts[1], oras.DefaultCopyOptions)
	return err
}

func addBlob(ctx context.Context, store *memory.Store, title, media string, data []byte) (ocispec.Descriptor, error) {
	desc := ocispec.Descriptor{
		MediaType: media,
		Digest:    godigest.FromBytes(data),
		Size:      int64(len(data)),
	}
	if title != "" {
		desc.Annotations = map[string]string{ocispec.AnnotationTitle: title}
	}
	if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}
