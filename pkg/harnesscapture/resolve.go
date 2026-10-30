package harnesscapture

import (
	"encoding/json"
	"errors"
	"slices"
)

// ResolveCredential installs explicit JSON bindings in a registered credential
// document. Ordinary settings are always literal, including strings containing
// braces or dollar signs. The caller supplies the sandbox's current sentinels.
func (d Definition) ResolveCredential(path, content string, sentinels map[string]string) (string, error) {
	if !slices.Contains(d.CredentialFiles, path) {
		return content, nil
	}
	var tree any
	if err := json.Unmarshal([]byte(content), &tree); err != nil {
		return "", errors.New("invalid credential JSON")
	}
	var resolve func(any) (any, error)
	resolve = func(v any) (any, error) {
		if name, ok := SecretReference(v); ok {
			if !d.secretPlaceholder(v) {
				return nil, errors.New("unregistered credential binding")
			}
			value := sentinels[name]
			if value == "" {
				return nil, errors.New("credential binding has no sentinel")
			}
			return value, nil
		}
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				next, err := resolve(child)
				if err != nil {
					return nil, err
				}
				v[k] = next
			}
		case []any:
			for k, child := range v {
				next, err := resolve(child)
				if err != nil {
					return nil, err
				}
				v[k] = next
			}
		}
		return v, nil
	}
	tree, err := resolve(tree)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(tree, "", "  ")
	return string(b) + "\n", err
}
