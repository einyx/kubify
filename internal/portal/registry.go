package portal

import (
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// TemplateMeta is the front-matter doc of a template file: it describes the
// template and its parameter defaults. The second doc is the manifest body —
// a text/template with a .Tenant placeholder.
type TemplateMeta struct {
	ID          string `yaml:"id"          json:"id"`
	Name        string `yaml:"name"        json:"name"`
	Description string `yaml:"description" json:"description"`
	Defaults    struct {
		Mode      string          `yaml:"mode"      json:"mode,omitempty"`
		Exclude   []string        `yaml:"exclude"   json:"exclude,omitempty"`
		Operators map[string]bool `yaml:"operators" json:"operators,omitempty"`
	} `yaml:"defaults" json:"defaults,omitempty"`
}

// Template is one creatable stack template. Body is never serialized to API
// responses — it is only rendered server-side.
type Template struct {
	Meta TemplateMeta `json:"-"`
	Body string       `json:"-"`

	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Description string                `json:"description"`
	Defaults    TemplateMeta_Defaults `json:"defaults"`
}

// TemplateMeta_Defaults mirrors TemplateMeta.Defaults for API responses.
type TemplateMeta_Defaults struct {
	Mode      string          `json:"mode,omitempty"`
	Exclude   []string        `json:"exclude,omitempty"`
	Operators map[string]bool `json:"operators,omitempty"`
}

const (
	configMapNamespace = "kubo-system"
	configMapName      = "portal-templates"
)

// builtinTemplates are compiled into the binary: generic skeletons only.
// Environment-specific templates live in the local templates dir
// (config/samples/portal-templates, gitignored) and/or the portal-templates
// ConfigMap.
//
//go:embed templates/*.yaml
var builtinFS embed.FS

// Registry resolves templates from three sources; later wins on id:
// embedded built-ins < local templates dir < portal-templates ConfigMap.
type Registry struct {
	localDir string
	client   client.Client
}

// NewRegistry builds a registry. localDir may be empty; client may be nil
// (the ConfigMap source is silently skipped).
func NewRegistry(localDir string, c client.Client) *Registry {
	return &Registry{localDir: localDir, client: c}
}

// List merges all sources, sorted by id.
func (r *Registry) List(ctx context.Context) ([]Template, error) {
	merged := map[string]Template{}

	builtins, err := loadEmbedTemplates()
	if err != nil {
		return nil, err
	}
	for id, t := range builtins {
		merged[id] = t
	}
	if r.localDir != "" {
		local, err := loadDirTemplates(r.localDir)
		if err != nil {
			return nil, err
		}
		for id, t := range local {
			merged[id] = t
		}
	}
	if r.client != nil {
		// The ConfigMap source is optional — never let a slow API path stall
		// the request (the caller's context may have no deadline).
		cmCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		cm, _ := loadConfigMapTemplates(cmCtx, r.client)
		cancel()
		for id, t := range cm {
			merged[id] = t
		}
	}

	ids := make([]string, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Template, 0, len(merged))
	for _, id := range ids {
		out = append(out, merged[id])
	}
	return out, nil
}

// Get resolves one template by id across all sources.
func (r *Registry) Get(ctx context.Context, id string) (Template, error) {
	all, err := r.List(ctx)
	if err != nil {
		return Template{}, err
	}
	for _, t := range all {
		if t.Meta.ID == id {
			return t, nil
		}
	}
	return Template{}, fmt.Errorf("unknown template %q", id)
}

func loadEmbedTemplates() (map[string]Template, error) {
	out := map[string]Template{}
	entries, err := builtinFS.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := builtinFS.ReadFile("templates/" + e.Name())
		if err != nil {
			return nil, err
		}
		t, err := parseTemplate(data)
		if err != nil {
			return nil, fmt.Errorf("builtin %s: %w", e.Name(), err)
		}
		out[t.Meta.ID] = t
	}
	return out, nil
}

func loadDirTemplates(dir string) (map[string]Template, error) {
	out := map[string]Template{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil // optional source
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yaml") && !strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		t, err := parseTemplate(data)
		if err != nil {
			return nil, fmt.Errorf("local %s: %w", e.Name(), err)
		}
		out[t.Meta.ID] = t
	}
	return out, nil
}

// loadConfigMapTemplates reads data values of kubo-system/portal-templates;
// each value is a full template file (metadata doc + body). Any error
// (absent RBAC, absent CM) yields an empty map — the source is optional.
func loadConfigMapTemplates(ctx context.Context, c client.Client) (map[string]Template, error) {
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: configMapNamespace, Name: configMapName}
	if err := c.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]Template{}, nil
		}
		return nil, err
	}
	out := map[string]Template{}
	for key, value := range cm.Data {
		t, err := parseTemplate([]byte(value))
		if err != nil {
			return nil, fmt.Errorf("configmap key %s: %w", key, err)
		}
		out[t.Meta.ID] = t
	}
	return out, nil
}

// parseTemplate splits the metadata doc from the manifest body.
func parseTemplate(data []byte) (Template, error) {
	docs := strings.SplitN(string(data), "\n---", 2)
	if len(docs) != 2 {
		return Template{}, fmt.Errorf("expected metadata doc, '---', then body")
	}
	var t Template
	if err := yaml.Unmarshal([]byte(docs[0]), &t.Meta); err != nil {
		return Template{}, fmt.Errorf("metadata: %w", err)
	}
	if t.Meta.ID == "" {
		return Template{}, fmt.Errorf("metadata missing id")
	}
	t.Body = strings.TrimSpace(docs[1])
	t.ID, t.Name, t.Description = t.Meta.ID, t.Meta.Name, t.Meta.Description
	t.Defaults.Mode = t.Meta.Defaults.Mode
	t.Defaults.Exclude = t.Meta.Defaults.Exclude
	t.Defaults.Operators = t.Meta.Defaults.Operators
	return t, nil
}
