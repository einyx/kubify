package writeback

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Location struct{ Repository, File, Revision string }

type Locator struct {
	Client     client.Client
	HTTPClient *http.Client
}

var kustomizationGVK = schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}
var gitRepositoryGVK = schema.GroupVersionKind{Group: "source.toolkit.fluxcd.io", Version: "v1", Kind: "GitRepository"}

func (l *Locator) Locate(ctx context.Context, stack *platformv1alpha1.Stack) (Location, error) {
	kn := stack.Labels["kustomize.toolkit.fluxcd.io/name"]
	kns := stack.Labels["kustomize.toolkit.fluxcd.io/namespace"]
	if kn == "" {
		return Location{}, fmt.Errorf("Stack is not managed by a Flux Kustomization")
	}
	if kns == "" {
		kns = "flux-system"
	}
	ks := &unstructured.Unstructured{}
	ks.SetGroupVersionKind(kustomizationGVK)
	if err := l.Client.Get(ctx, client.ObjectKey{Namespace: kns, Name: kn}, ks); err != nil {
		return Location{}, err
	}
	sourceName, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "name")
	sourceNS, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "namespace")
	if sourceNS == "" {
		sourceNS = kns
	}
	root, _, _ := unstructured.NestedString(ks.Object, "spec", "path")
	root = strings.TrimPrefix(path.Clean(root), "./")
	gr := &unstructured.Unstructured{}
	gr.SetGroupVersionKind(gitRepositoryGVK)
	if err := l.Client.Get(ctx, client.ObjectKey{Namespace: sourceNS, Name: sourceName}, gr); err != nil {
		return Location{}, err
	}
	repo, _, _ := unstructured.NestedString(gr.Object, "spec", "url")
	artifact, _, _ := unstructured.NestedString(gr.Object, "status", "artifact", "url")
	revision, _, _ := unstructured.NestedString(gr.Object, "status", "artifact", "revision")
	if repo == "" || artifact == "" {
		return Location{}, fmt.Errorf("Flux source has no ready artifact")
	}
	hc := l.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, artifact, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return Location{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Location{}, fmt.Errorf("fetch Flux artifact: %s", resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return Location{}, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Location{}, err
		}
		name := strings.TrimPrefix(path.Clean(h.Name), "./")
		if root != "." && root != "" && !strings.HasPrefix(name, root+"/") {
			continue
		}
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return Location{}, err
		}
		if yamlContainsStack(b, stack.Namespace, stack.Name) {
			return Location{Repository: repo, File: name, Revision: revision}, nil
		}
	}
	return Location{}, fmt.Errorf("source manifest for Stack %s/%s not found in Flux artifact", stack.Namespace, stack.Name)
}

func yamlContainsStack(b []byte, namespace, name string) bool {
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	for {
		var n map[string]interface{}
		if err := dec.Decode(&n); err != nil {
			return false
		}
		if n["apiVersion"] == "platform.kubo.io/v1alpha1" && n["kind"] == "Stack" {
			m, _ := n["metadata"].(map[string]interface{})
			ns, _ := m["namespace"].(string)
			if ns == "" {
				ns = "default"
			}
			if m["name"] == name && ns == namespace {
				return true
			}
		}
	}
}
