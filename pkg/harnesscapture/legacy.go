package harnesscapture

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// UpgradeLegacyJSON converts persisted delivery data without executing legacy
// templates. Only the shipped trust suffixes and exact credential references
// are recognized. Unknown executable content is refused rather than discarded.
func UpgradeLegacyJSON(data []byte) ([]byte, error) {
	if !strings.Contains(string(data), `"template"`) && !strings.Contains(string(data), `"templateSuffix"`) {
		return data, nil
	}
	var root any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	var upgrade func(any) (any, error)
	upgrade = func(v any) (any, error) {
		switch obj := v.(type) {
		case []any:
			result := make([]any, 0, len(obj))
			for _, child := range obj {
				original := child
				child, err := upgrade(child)
				if err != nil {
					return nil, err
				}
				if child != nil || original == nil {
					result = append(result, child)
				}
			}
			return result, nil
		case map[string]any:
			if content, ok := obj["content"].(string); ok && obj["template"] == true {
				path, _ := obj["path"].(string)
				if path == ".claude.json" && content == legacyClaudeSeed {
					return nil, nil
				}
				if path == ".codex/config.toml" {
					for _, suffix := range legacyCodexTrust {
						if strings.HasSuffix(strings.TrimSpace(content), strings.TrimSpace(suffix)) {
							content = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(content), strings.TrimSpace(suffix)))
							break
						}
					}
					if strings.Contains(content, "{{") {
						return nil, errors.New("unrecognized legacy harness settings template")
					}
					if content != "" {
						content += "\n"
					}
				} else {
					var doc any
					if json.Unmarshal([]byte(content), &doc) != nil {
						return nil, errors.New("legacy harness credential template is not JSON")
					}
					var convert func(any) (any, error)
					convert = func(v any) (any, error) {
						switch x := v.(type) {
						case string:
							if m := legacySecret.FindStringSubmatch(x); len(m) == 2 {
								return map[string]any{"$secret": m[1]}, nil
							}
							if strings.Contains(x, "{{") {
								return nil, errors.New("unrecognized legacy harness credential template")
							}
						case map[string]any:
							for k, c := range x {
								n, e := convert(c)
								if e != nil {
									return nil, e
								}
								x[k] = n
							}
						case []any:
							for k, c := range x {
								n, e := convert(c)
								if e != nil {
									return nil, e
								}
								x[k] = n
							}
						}
						return v, nil
					}
					doc, err := convert(doc)
					if err != nil {
						return nil, err
					}
					b, err := json.MarshalIndent(doc, "", "  ")
					if err != nil {
						return nil, err
					}
					content = string(b) + "\n"
				}
				obj["content"] = content
				delete(obj, "template")
			}
			// Metadata's old Codex suffix has no meaning in literal settings.
			delete(obj, "templateSuffix")
			for k, child := range obj {
				n, err := upgrade(child)
				if err != nil {
					return nil, err
				}
				obj[k] = n
			}
		}
		return v, nil
	}
	root, err := upgrade(root)
	if err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

var legacySecret = regexp.MustCompile(`^\{\{\s*\.secrets\.([A-Z_][A-Z0-9_]*)\s*\}\}$`)

const legacySourceTrust = `{{- range .sources }}{{- if eq .slug "primary" }}
[projects.{{ .target | json }}]
trust_level = "trusted"
{{- end }}{{- end }}`
const legacyWorkingTrust = `[projects.{{ .workingDir | json }}]
trust_level = "trusted"`

var legacyCodexTrust = []string{
	"{{- if .workingDir }}\n" + legacyWorkingTrust + "\n{{- else }}" + legacySourceTrust + "{{- end }}",
	"{{- if .workingDir }}\n\n" + legacyWorkingTrust + "\n{{- end }}",
	"{{- if .workingDir }}\n" + legacyWorkingTrust + "\n{{- end }}",
	legacySourceTrust, legacyWorkingTrust,
}

const legacyClaudeSeed = "{\n  \"hasCompletedOnboarding\": true,\n  \"hasSeenAutoDefaultNudge\": true\n  {{- if .secrets.ANTHROPIC_API_KEY }},\n  \"primaryApiKey\": {{ .secrets.ANTHROPIC_API_KEY | json }}\n  {{- end }},\n  \"projects\": {\n    {{ .workingDir | json }}: {\n      \"hasTrustDialogAccepted\": true\n    }\n  }\n}"
