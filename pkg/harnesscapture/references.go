package harnesscapture

import (
	"encoding/json"
	"errors"
	"slices"
)

// ReferencedSecrets names sentinel variables used by validated credential
// templates. Storage owners must also check these variables have bindings; a
// registered provider declaration alone does not supply a credential.
func (d Definition) ReferencedSecrets(files []File) ([]string, error) {
	refs := map[string]bool{}
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case string:
			if m := secretTemplate.FindStringSubmatch(v); len(m) == 2 {
				refs[m[1]] = true
			}
		case map[string]any:
			for _, child := range v {
				visit(child)
			}
		case []any:
			for _, child := range v {
				visit(child)
			}
		}
	}
	for _, file := range files {
		if !slices.Contains(d.CredentialFiles, file.Path) {
			continue
		}
		var tree any
		if err := json.Unmarshal([]byte(file.Content), &tree); err != nil {
			return nil, errors.New("invalid credential template")
		}
		visit(tree)
	}
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}
