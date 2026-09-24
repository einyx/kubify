package controller

import (
	"encoding/json"
	"fmt"
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

// applyFeatureFlags compiles stack.spec.featureFlags into the frontend
// component's env.feature_flags, with the highest precedence (above
// componentValues). Keys may be given in the chart's lowercase snake_case
// form or as the rendered env name (MX_FF_CONNECTORS_ENABLED); both are
// normalized to the chart's key. Only components that consume
// env.feature_flags (the frontend chart) are targeted, so unknown
// components are left untouched.
func applyFeatureFlags(featureFlags map[string]string, compName string, values map[string]interface{}) {
	if len(featureFlags) == 0 || (compName != "frontend" && compName != "foundation-frontend") {
		return
	}
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
	for k, v := range featureFlags {
		ff[normalizeFeatureFlagKey(k)] = v
	}
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
