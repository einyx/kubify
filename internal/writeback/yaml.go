package writeback

import (
	"bytes"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

// MutateFeatureFlags updates only spec.featureFlags in the matching Stack
// document while retaining comments and the rest of the YAML node tree.
func MutateFeatureFlags(src []byte, namespace, name string, expected, desired map[string]string) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var docs []*yaml.Node
	found := false
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		docs = append(docs, &doc)
		root := doc.Content[0]
		if scalar(root, "apiVersion") != "platform.kubo.io/v1alpha1" || scalar(root, "kind") != "Stack" {
			continue
		}
		meta := mapping(root, "metadata")
		if meta == nil || scalar(meta, "name") != name {
			continue
		}
		ns := scalar(meta, "namespace")
		if ns == "" {
			ns = "default"
		}
		if ns != namespace {
			continue
		}
		spec := ensureMapping(root, "spec")
		flags := mapping(spec, "featureFlags")
		current := stringMap(flags)
		for k, v := range expected {
			if current[k] != v {
				return nil, fmt.Errorf("feature flag %s changed from expected %q to %q", k, v, current[k])
			}
		}
		setStringMap(spec, "featureFlags", desired)
		found = true
	}
	if !found {
		return nil, fmt.Errorf("Stack %s/%s not found", namespace, name)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
	}
	enc.Close()
	return out.Bytes(), nil
}

func mapping(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func scalar(n *yaml.Node, key string) string {
	if v := mapping(n, key); v != nil {
		return v.Value
	}
	return ""
}
func ensureMapping(n *yaml.Node, key string) *yaml.Node {
	if v := mapping(n, key); v != nil {
		return v
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	n.Content = append(n.Content, k, v)
	return v
}
func stringMap(n *yaml.Node) map[string]string {
	out := map[string]string{}
	if n == nil {
		return out
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1].Value
	}
	return out
}
func setStringMap(n *yaml.Node, key string, m map[string]string) {
	v := mapping(n, key)
	if v == nil {
		v = ensureMapping(n, key)
	}
	v.Kind = yaml.MappingNode
	v.Tag = "!!map"
	v.Content = nil
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v.Content = append(v.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: m[k]})
	}
}
