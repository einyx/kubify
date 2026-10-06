package writeback

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MutateFeatureFlags updates only spec.featureFlags in the matching Stack
// document while retaining comments and the rest of the YAML node tree.
func MutateFeatureFlags(src []byte, namespace, name string, expected, desired map[string]string) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	found := false
	start, end, indent := -1, -1, ""
	var order []string
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
		key := mappingKey(spec, "featureFlags")
		if key == nil {
			return nil, fmt.Errorf("Stack %s/%s has no featureFlags block", namespace, name)
		}
		start = key.Line - 1
		indent = strings.Repeat(" ", key.Column-1)
		end = blockEnd(src, start, key.Column-1)
		if flags != nil {
			for i := 0; i+1 < len(flags.Content); i += 2 {
				order = append(order, flags.Content[i].Value)
			}
		}
		found = true
		break
	}
	if !found {
		return nil, fmt.Errorf("Stack %s/%s not found", namespace, name)
	}
	lines := strings.Split(string(src), "\n")
	seen := map[string]bool{}
	var replacement []string
	replacement = append(replacement, indent+"featureFlags:")
	for _, k := range order {
		if v, ok := desired[k]; ok {
			replacement = append(replacement, indent+"  "+k+": '"+v+"'")
			seen[k] = true
		}
	}
	var extra []string
	for k := range desired {
		if !seen[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		replacement = append(replacement, indent+"  "+k+": '"+desired[k]+"'")
	}
	lines = append(lines[:start], append(replacement, lines[end:]...)...)
	return []byte(strings.Join(lines, "\n")), nil
}

func mappingKey(n *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i]
		}
	}
	return nil
}
func blockEnd(src []byte, start, indent int) int {
	lines := strings.Split(string(src), "\n")
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		spaces := len(line) - len(strings.TrimLeft(line, " "))
		if spaces <= indent {
			return i
		}
	}
	return len(lines)
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
