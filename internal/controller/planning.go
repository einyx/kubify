package controller

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func toMap(in *apiextensionsv1.JSON) map[string]interface{} {
	if in == nil || len(in.Raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(in.Raw, &m); err != nil {
		return nil
	}
	return m
}

// mergeValues deep-merges src into dst (dst wins).
func mergeValues(dst, src map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if sm, ok := v.(map[string]interface{}); ok {
			if dm, ok := out[k].(map[string]interface{}); ok {
				out[k] = mergeValues(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// resolveComponentValues merges, in precedence order (lowest first):
//  1. StackDefinition component defaults
//  2. Stack.spec.values["components"][<name>] (stripped of the prefix)
//  3. Stack.spec.componentValues[<name>]
func resolveComponentValues(
	defDefaults *apiextensionsv1.JSON,
	stackValues *apiextensionsv1.JSON,
	componentValues map[string]apiextensionsv1.JSON,
	compName string,
) map[string]interface{} {
	merged := map[string]interface{}{}
	if d := toMap(defDefaults); d != nil {
		merged = mergeValues(merged, d)
	}
	if sv := toMap(stackValues); sv != nil {
		if comps, ok := sv["components"].(map[string]interface{}); ok {
			if cv, ok := comps[compName].(map[string]interface{}); ok {
				merged = mergeValues(merged, cv)
			}
		}
	}
	if cv := toMap(ptr(componentValues, compName)); cv != nil {
		merged = mergeValues(merged, cv)
	}
	return merged
}

// applyFeatureFlags compiles stack.spec.featureFlags into the consuming
// components' values, with the highest precedence (above componentValues):
// the frontend chart's env.feature_flags map, and the backend's MX_FF_*
// env entries — the backend exposes every ff_* Config field as an MX_FF_*
// boolean (see its GET /flags introspection endpoint), so one flag map
// drives both tiers. Keys may be given in the chart's lowercase snake_case
// form or as the rendered env name (MX_FF_CONNECTORS_ENABLED); both are
// normalized. Other components are left untouched.
func applyFeatureFlags(featureFlags map[string]string, compName string, values map[string]interface{}) {
	if len(featureFlags) == 0 {
		return
	}
	switch compName {
	case "frontend", "foundation-frontend":
		env, _ := values["env"].(map[string]interface{})
		if env == nil {
			env = map[string]interface{}{}
			values["env"] = env
		}
		ff, _ := env["feature_flags"].(map[string]interface{})
		if ff == nil {
			ff = map[string]interface{}{}
			env["feature_flags"] = ff
		}
		for _, k := range sortedKeys(featureFlags) {
			v := featureFlags[k]
			ff[normalizeFeatureFlagKey(k)] = v
		}
	case "backend", "foundation-backend":
		// The backend chart's env is a {name, value} list. Same-named
		// entries from componentValues are replaced — featureFlags win.
		env, _ := values["env"].([]interface{})
		idx := map[string]int{}
		for i, e := range env {
			m, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			if n, ok := m["name"].(string); ok {
				idx[n] = i
			}
		}
		for _, k := range sortedKeys(featureFlags) {
			v := featureFlags[k]
			name := "MX_FF_" + strings.ToUpper(normalizeFeatureFlagKey(k))
			entry := map[string]interface{}{"name": name, "value": v}
			if i, ok := idx[name]; ok {
				env[i] = entry
			} else {
				env = append(env, entry)
			}
		}
		values["env"] = env
	}
}

// sortedKeys gives deterministic iteration over a flag map so rendered
// values — and therefore pod templates — are byte-stable across reconciles.
// Without it Go's randomized map order reorders env entries every pass and
// the Deployment flaps into a new ReplicaSet on each reconcile.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// applyImageTags compiles stack.spec.imageTags into each component's
// image.tag, with the highest precedence (above componentValues). Keys are
// component names; only components present in the map are touched, so a
// typo'd name is a no-op rather than a broken chart.
func applyImageTags(imageTags map[string]string, compName string, values map[string]interface{}) {
	if len(imageTags) == 0 {
		return
	}
	tag, ok := imageTags[compName]
	if !ok || tag == "" {
		return
	}
	image, _ := values["image"].(map[string]interface{})
	if image == nil {
		image = map[string]interface{}{}

		values["image"] = image
	}
	image["tag"] = tag
}

// canonicalizeEnvLists sorts every {name,value} env list under values by
// name. Env entries may be assembled from several sources (component
// values, stack values, componentValues, featureFlags); any map-keyed
// intermediate shuffles them, and a shuffled list changes the rendered
// manifest — defeating helm's no-op detection and rolling the workload on
// every reconcile. Sorting at the helm boundary makes renders byte-stable.
func canonicalizeEnvLists(values map[string]interface{}) {
	env, ok := values["env"].([]interface{})
	if !ok || len(env) < 2 {
		return
	}
	entries := make([]struct {
		name string
		raw  interface{}
	}, 0, len(env))
	for _, e := range env {
		m, ok := e.(map[string]interface{})
		if !ok {
			return // not a name-keyed list; leave untouched
		}
		n, _ := m["name"].(string)
		entries = append(entries, struct {
			name string
			raw  interface{}
		}{n, e})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	sorted := make([]interface{}, len(entries))
	for i, e := range entries {
		sorted[i] = e.raw
	}
	values["env"] = sorted
}

// normalizeFeatureFlagKey converts MX_FF_UPPER_SNAKE to the chart's
// lowercase snake_case key. Already-normalized keys pass through.
func normalizeFeatureFlagKey(k string) string {
	k = strings.TrimPrefix(k, "MX_FF_")
	return strings.ToLower(k)
}

// defaultFrontendBaseURL fills values env.auth0.baseurl from the component's
// virtualService.host when the operator renders the frontend component and no
// explicit value is set. Tenant URLs are derived from the tenant name this
// way — any namespace a user spins gets a correct APP_BASE_URL with zero
// per-tenant secret configuration. The "{{ namespace }}" placeholder used by
// the Flux-convention charts is resolved against the stack namespace.
func defaultFrontendBaseURL(stackNamespace string, values map[string]interface{}) {
	if values == nil {
		return
	}
	vs, _ := values["virtualService"].(map[string]interface{})
	if vs == nil {
		return
	}
	host, _ := vs["host"].(string)
	host = strings.TrimSpace(strings.ReplaceAll(host, "{{ namespace }}", stackNamespace))
	if host == "" {
		return
	}
	env, _ := values["env"].(map[string]interface{})
	if env == nil {
		env = map[string]interface{}{}
		values["env"] = env
	}
	auth0, _ := env["auth0"].(map[string]interface{})
	if auth0 == nil {
		auth0 = map[string]interface{}{}
		env["auth0"] = auth0
	}
	if s, _ := auth0["baseurl"].(string); strings.TrimSpace(s) != "" {
		return // explicit value wins
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	auth0["baseurl"] = strings.TrimSuffix(host, "/") + "/"
}

// topoOrder returns component names ordered so dependencies come first.
// Returns an error on missing or cyclic dependencies.
func topoOrder(comps []platformv1alpha1.StackComponentSpec) ([]string, error) {
	byName := map[string]platformv1alpha1.StackComponentSpec{}
	for _, c := range comps {
		if _, dup := byName[c.Name]; dup {
			return nil, fmt.Errorf("duplicate component %q", c.Name)
		}
		byName[c.Name] = c
	}

	var order []string
	state := map[string]int{} // 0=unvisited 1=visiting 2=done

	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("dependency cycle involving %q", name)
		case 2:
			return nil
		}
		state[name] = 1
		c, ok := byName[name]
		if !ok {
			return fmt.Errorf("unknown component %q", name)
		}
		for _, dep := range c.DependsOn {
			if _, ok := byName[dep]; !ok {
				return fmt.Errorf("component %q depends on unknown component %q", name, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		for _, dep := range c.DependsOnReady {
			if _, ok := byName[dep]; !ok {
				return fmt.Errorf("component %q depends on unknown component %q (dependsOnReady)", name, dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[name] = 2
		order = append(order, name)
		return nil
	}

	for _, c := range comps {
		if err := visit(c.Name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func ptr(m map[string]apiextensionsv1.JSON, k string) *apiextensionsv1.JSON {
	if v, ok := m[k]; ok {
		return &v
	}
	return nil
}

// applyChartVersion compiles spec.chartVersions into a component's chart
// pin with the highest precedence, mirroring applyImageTags. Only meaningful
// for components that pull charts from an OCI repo; bundle-supplied charts
// ignore it (the bundle packages exactly one version).
func applyChartVersion(chartVersions map[string]string, compName string, ref *platformv1alpha1.ChartRef) {
	if len(chartVersions) == 0 || ref == nil {
		return
	}
	if v, ok := chartVersions[compName]; ok && v != "" {
		ref.ChartVersion = v
	}
}
