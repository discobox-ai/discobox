package harnesscapture

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

// NormalizeSettings accepts ordinary settings and rejects known excluded paths.
// The resulting JSON is canonical regardless of the input format. Exporters
// preserve the harness's file format; validation compares its parsed structure.
func NormalizeSettings(s Setting, content string) (map[string]any, error) {
	if s.TemplateSuffix != "" {
		if !strings.HasSuffix(content, s.TemplateSuffix) {
			return nil, errors.New("settings lack registered template suffix")
		}
		content = strings.TrimSuffix(content, s.TemplateSuffix)
		if strings.Contains(content, "{{") {
			return nil, errors.New("settings prefix contains an unregistered template action")
		}
	}
	var value map[string]any
	switch s.Format {
	case "json", "jsonc":
		// Exporters remove JSONC comments; wire values are strict JSON.
		if err := json.Unmarshal([]byte(content), &value); err != nil {
			return nil, errors.New("invalid settings JSON")
		}
	case "toml":
		if err := toml.Unmarshal([]byte(content), &value); err != nil {
			return nil, errors.New("invalid settings TOML")
		}
	case "yaml":
		if err := yaml.Unmarshal([]byte(content), &value); err != nil {
			return nil, errors.New("invalid settings YAML")
		}
	default:
		return nil, errors.New("unsupported settings format")
	}
	// TOML arrays of tables decode as []map[string]any, while YAML can
	// produce maps with non-string keys. Normalize the entire tree before
	// checking exclusions so no parser-specific container bypasses traversal.
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("settings must contain JSON-compatible objects")
	}
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, errors.New("invalid settings structure")
	}
	if len(s.Fields) != 0 {
		return nil, errors.New("legacy settings allowlist: refresh the harness bundle metadata")
	}
	if value == nil {
		return nil, errors.New("settings must be an object")
	}
	if err := validateSettingsTree(value, nil, s.Exclude); err != nil {
		return nil, err
	}
	return value, nil
}

func validateSettingsTree(value any, keys []string, excluded []string) error {
	for _, pattern := range excluded {
		if settingsPathMatches(strings.Split(pattern, "."), keys) {
			return errors.New("settings contain an excluded credential or local field")
		}
	}
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "__proto__" || key == "constructor" || key == "prototype" {
				return errors.New("unsafe settings key")
			}
			if err := validateSettingsTree(child, append(slices.Clone(keys), key), excluded); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range v {
			if err := validateSettingsTree(child, append(slices.Clone(keys), fmt.Sprint(i)), excluded); err != nil {
				return err
			}
		}
	}
	return nil
}

func settingsPathMatches(pattern, keys []string) bool {
	if len(pattern) == 0 {
		return len(keys) == 0
	}
	if pattern[0] == "**" {
		return settingsPathMatches(pattern[1:], keys) || (len(keys) > 0 && settingsPathMatches(pattern, keys[1:]))
	}
	return len(keys) > 0 && (pattern[0] == "*" || pattern[0] == keys[0]) && settingsPathMatches(pattern[1:], keys[1:])
}
func scalarType(v any, kind string) bool {
	switch kind {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		switch v.(type) {
		case float64, int, int64, uint64:
			return true
		}
	case "string-array":
		rv := reflect.ValueOf(v)
		if rv.Kind() != reflect.Slice {
			return false
		}
		for i := 0; i < rv.Len(); i++ {
			if _, ok := rv.Index(i).Interface().(string); !ok {
				return false
			}
		}
		return true
	}
	return false
}

func (d Definition) ValidateFiles(files []File) error {
	if len(files) > 128 {
		return errors.New("too many capture files")
	}
	seen := map[string]bool{}
	size := 0
	for _, f := range files {
		size += len(f.Content)
		if size > MaxBytes || !HomePath(f.Path) || seen[f.Path] {
			return errors.New("invalid capture files")
		}
		seen[f.Path] = true
		var setting *Setting
		for i := range d.Settings {
			if d.Settings[i].Path == f.Path {
				setting = &d.Settings[i]
				break
			}
		}
		if setting != nil {
			if f.Template != (setting.TemplateSuffix != "") || f.CreateOnly != setting.CreateOnly {
				return errors.New("invalid portable settings flags")
			}
			if _, err := NormalizeSettings(*setting, f.Content); err != nil {
				return err
			}
			continue
		}
		if !slices.Contains(d.CredentialFiles, f.Path) || !f.Template {
			return errors.New("capture file is not registered")
		}
		var credentialSchema *Setting
		for i := range d.CredentialSettings {
			if d.CredentialSettings[i].Path == f.Path {
				credentialSchema = &d.CredentialSettings[i]
				break
			}
		}
		if credentialSchema == nil {
			return errors.New("credential file has no registered field schema")
		}
		var document any
		if err := json.Unmarshal([]byte(f.Content), &document); err != nil {
			return errors.New("credential template must be JSON")
		}
		if err := d.validateCredentialTree(document, "", credentialSchema.Fields); err != nil {
			return err
		}
	}
	return nil
}

func (d Definition) ValidateCandidate(c Candidate) error {
	b, err := json.Marshal(c)
	if err != nil || len(b) > MaxBytes {
		return errors.New("capture exceeds size limit")
	}
	if c.ID == "" || c.Baseline.HarnessConfigID == "" || c.WriteRevision < 1 {
		return errors.New("invalid capture identity")
	}
	seen := map[string]bool{}
	files := []File{}
	for _, change := range c.Files {
		if !HomePath(change.Path) || seen[change.Path] || change.Previous != nil {
			return errors.New("invalid capture file change")
		}
		seen[change.Path] = true
		if change.Remove {
			if change.File != nil {
				return errors.New("removed capture file has content")
			}
			allowed := false
			for _, s := range d.Settings {
				allowed = allowed || s.Path == change.Path
			}
			if !allowed {
				return errors.New("only portable settings can be removed")
			}
		} else {
			if change.File == nil || change.File.Path != change.Path {
				return errors.New("capture file path mismatch")
			}
			files = append(files, *change.File)
		}
	}
	if err := d.ValidateFiles(files); err != nil {
		return err
	}
	seen = map[string]bool{}
	for _, c := range c.Credentials {
		p, ok := d.Provider(c.Provider, c.EnvName)
		if !ok || !slices.Contains(p.Types, c.Type) || seen[c.EnvName] {
			return errors.New("unregistered or duplicate capture credential")
		}
		seen[c.EnvName] = true
		if c.Keep {
			if len(c.Value) > 0 && string(c.Value) != "null" {
				return errors.New("kept credential contains value")
			}
			continue
		}
		var v struct {
			Token                string   `json:"token"`
			RefreshToken         string   `json:"refreshToken"`
			TokenURL             string   `json:"tokenUrl"`
			ClientID             string   `json:"clientId"`
			AccessTokenExpiresAt int64    `json:"accessTokenExpiresAt"`
			TokenRequestEncoding string   `json:"tokenRequestEncoding"`
			Scopes               []string `json:"scopes"`
			SubscriptionType     string   `json:"subscriptionType"`
		}
		dec := json.NewDecoder(bytes.NewReader(c.Value))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&v); err != nil {
			return errors.New("invalid capture credential value")
		}
		if v.Token == "" || strings.Contains(v.Token, "{{") || strings.Contains(v.RefreshToken, "{{") {
			return errors.New("capture credential is missing or a placeholder")
		}
		if c.Type == "oauth" {
			if p.OAuth == nil || v.RefreshToken == "" {
				return errors.New("OAuth capture requires a refresh token")
			}
			if v.TokenURL != "" && v.TokenURL != p.OAuth.TokenURL || v.ClientID != "" && v.ClientID != p.OAuth.ClientID || v.TokenRequestEncoding != "" && v.TokenRequestEncoding != p.OAuth.Encoding {
				return errors.New("OAuth metadata differs from registered provider")
			}
		} else if v.RefreshToken != "" || v.TokenURL != "" || v.ClientID != "" {
			return fmt.Errorf("token credential contains OAuth data")
		}
	}
	return nil
}

var secretTemplate = regexp.MustCompile(`^\{\{\s*\.secrets\.([A-Z_][A-Z0-9_]*)\s*\}\}$`)

func (d Definition) secretPlaceholder(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	m := secretTemplate.FindStringSubmatch(s)
	if len(m) != 2 {
		return false
	}
	for _, p := range d.Providers {
		if _, ok := d.Provider(p.ID, m[1]); ok {
			return true
		}
	}
	return false
}
func (d Definition) validateCredentialTree(v any, field string, fields map[string]string) error {
	kind, found := fields[field]
	if !found {
		for pattern, k := range fields {
			a, b := strings.Split(pattern, "."), strings.Split(field, ".")
			if len(a) != len(b) {
				continue
			}
			match := true
			for i := range a {
				if a[i] != "*" && a[i] != b[i] {
					match = false
					break
				}
			}
			if match {
				kind = k
				found = true
				break
			}
		}
	}
	if found {
		valid := scalarType(v, kind)
		switch kind {
		case "secret":
			valid = d.secretPlaceholder(v)
		case "secret-or-null":
			valid = v == nil || d.secretPlaceholder(v)
		case "placeholder":
			valid = safePlaceholder(v)
		case "secret-or-placeholder":
			valid = d.secretPlaceholder(v) || safePlaceholder(v)
		case "id-token":
			valid = safeIDToken(v)
		}
		if !valid {
			return errors.New("invalid credential template leaf")
		}
		return nil
	}
	switch value := v.(type) {
	case map[string]any:
		for key, item := range value {
			next := key
			if field != "" {
				next = field + "." + key
			}
			if err := d.validateCredentialTree(item, next, fields); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range value {
			next := fmt.Sprint(i)
			if field != "" {
				next = fmt.Sprintf("%s.%d", field, i)
			}
			if err := d.validateCredentialTree(item, next, fields); err != nil {
				return err
			}
		}
	default:
		return errors.New("credential template contains unregistered fields")
	}
	return nil
}
func safePlaceholder(v any) bool {
	s, ok := v.(string)
	return ok && (s == "discobox-refresh-happens-in-the-control-plane" || s == "discobox-access-happens-in-the-control-plane" || s == "discobox-copilot-token-is-exchanged-in-the-sandbox" || s == "")
}
func safeIDToken(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 || (parts[2] != "" && parts[2] != "discobox") {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return false
	}
	var header map[string]any
	if json.Unmarshal(b, &header) != nil || header["alg"] != "none" {
		return false
	}
	b, e = base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return false
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil || len(b) >= 8192 {
		return false
	}
	for key, value := range claims {
		switch key {
		case "email":
			if _, ok := value.(string); !ok {
				return false
			}
		case "https://api.openai.com/auth":
			auth, ok := value.(map[string]any)
			if !ok {
				return false
			}
			for field, value := range auth {
				switch field {
				case "chatgpt_plan_type", "chatgpt_user_id", "chatgpt_account_id":
					if _, ok := value.(string); !ok {
						return false
					}
				case "chatgpt_account_is_fedramp":
					if _, ok := value.(bool); !ok {
						return false
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}
