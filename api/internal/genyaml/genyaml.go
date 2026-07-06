// Package genyaml provides shared OpenAPI YAML loading helpers for the api
// codegen tools (gensandboxopenapi, genmanifestmodels).
package genyaml

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// ReadYAML reads and normalizes a YAML document into a map[string]any tree.
func ReadYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out, ok := Normalize(raw).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s did not decode to an object", path)
	}
	return out, nil
}

// Normalize recursively converts map[any]any nodes (from yaml.v3) into
// map[string]any so the tree can be handled uniformly.
func Normalize(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[key] = Normalize(child)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			out[fmt.Sprint(key)] = Normalize(child)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, child := range value {
			out[i] = Normalize(child)
		}
		return out
	default:
		return value
	}
}

// AsMap type-asserts value as a map[string]any.
func AsMap(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

// SortedKeys returns the keys of m in sorted order.
func SortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
