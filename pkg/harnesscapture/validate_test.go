package harnesscapture

import (
	"encoding/json"
	"testing"
)

func TestCredentialTemplatesRejectPlaintextAndUnknownLeaves(t *testing.T) {
	d := Definition{Providers: []Provider{{ID: "vendor", EnvName: "API_KEY", Types: []string{"token"}}}, CredentialFiles: []string{"auth.json"}, CredentialSettings: []Setting{{Path: "auth.json", Fields: map[string]string{"token": "secret", "account": "string"}}}}
	for _, tc := range []struct {
		content string
		valid   bool
	}{{`{"token":{"$secret":"API_KEY"},"account":"me"}`, true}, {`{"token":"real-secret"}`, false}, {`{"token":{"$secret":"OTHER"}}`, false}, {`{"token":{"$secret":"API_KEY"},"hidden":"real-secret"}`, false}} {
		err := d.ValidateFiles([]File{{Path: "auth.json", Content: tc.content}})
		if (err == nil) != tc.valid {
			t.Fatalf("template acceptance=%v, expected %v", err == nil, tc.valid)
		}
	}
}
func TestSettingsRejectKnownCredentialFields(t *testing.T) {
	for _, s := range []Setting{{Format: "json", Exclude: []string{"apiKey", "projects"}}, {Format: "toml", Exclude: []string{"apiKey", "projects"}}, {Format: "yaml", Exclude: []string{"apiKey", "projects"}}} {
		content := `{"model":"a","apiKey":"secret"}`
		if s.Format == "toml" {
			content = "model = 'a'\napiKey = 'secret'"
		}
		if s.Format == "yaml" {
			content = "model: a\napiKey: secret"
		}
		if _, err := NormalizeSettings(s, content); err == nil {
			t.Fatalf("accepted credential field in %s", s.Format)
		}
	}
}
func TestCaptureRejectsSandboxOAuthMetadata(t *testing.T) {
	// #nosec G101 -- synthetic provider metadata, no credential.
	d := Definition{Providers: []Provider{{ID: "vendor", EnvName: "TOKEN", Types: []string{"oauth"}, OAuth: &OAuth{TokenURL: "https://vendor.example/token", ClientID: "registered"}}}}
	c := Candidate{ID: "candidate", Baseline: Baseline{HarnessConfigID: "config"}, WriteRevision: 1, Credentials: []Credential{{Provider: "vendor", EnvName: "TOKEN", Type: "oauth", Value: json.RawMessage(`{"token":"access","refreshToken":"refresh","tokenUrl":"https://attacker.example/"}`)}}}
	if err := d.ValidateCandidate(c); err == nil {
		t.Fatal("accepted sandbox-selected refresh endpoint")
	}
	c.Credentials[0].Value = json.RawMessage(`{"token":"access","refreshToken":"refresh"}`)
	if err := d.ValidateCandidate(c); err != nil {
		t.Fatal(err)
	}
}

func TestPortableTrustSuffixIsRegisteredAndNotCapturedFromWorkspace(t *testing.T) {
	d := Definition{Settings: []Setting{{Path: ".codex/config.toml", Format: "toml", Exclude: []string{"apiKey", "projects"}}}}
	f := File{Path: ".codex/config.toml", Content: "model = \"next\"\n"}
	if err := d.ValidateFiles([]File{f}); err != nil {
		t.Fatal(err)
	}
	f.Content = "model = \"next\"\n[projects.\"/private/workspace\"]\ntrust_level = \"trusted\"\n"
	if err := d.ValidateFiles([]File{f}); err == nil {
		t.Fatal("accepted captured workspace trust")
	}
}

func TestSettingsPreserveUnknownPreferencesAndNestedStructures(t *testing.T) {
	s := Setting{Format: "json", Exclude: []string{"env", "provider.*.options.apiKey", "mcp.*.headers"}}
	body := `{"theme":"light","newPreference":{"nested":[1,true,{"accent":"blue"}]},"hooks":{"event":[{"command":"notify"}]}}`
	got, err := NormalizeSettings(s, body)
	if err != nil || got["theme"] != "light" {
		t.Fatalf("preferences: %v, %v", got, err)
	}
	for _, body := range []string{`{"env":{"VENDOR_KEY":"secret"}}`, `{"provider":{"vendor":{"options":{"apiKey":"secret"}}}}`, `{"mcp":{"local":{"headers":{"Authorization":"secret"}}}}`} {
		if _, err := NormalizeSettings(s, body); err == nil {
			t.Fatalf("accepted excluded credentials: %s", body)
		}
	}
}

func TestLegacySettingsAllowlistRequiresMetadataRefresh(t *testing.T) {
	if _, err := NormalizeSettings(Setting{Format: "json", Fields: map[string]string{"theme": "string"}}, `{}`); err == nil {
		t.Fatal("silently interpreted legacy allowlist as unrestricted settings")
	}
}

func TestSettingsExclusionsTraverseParserSpecificContainers(t *testing.T) {
	for _, tc := range []struct{ format, body string }{
		{"toml", "[[providers]]\napiKey = \"secret\"\n"},
		{"yaml", "providers:\n  123:\n    apiKey: secret\n"},
	} {
		if _, err := NormalizeSettings(Setting{Format: tc.format, Exclude: []string{"providers.*.apiKey"}}, tc.body); err == nil {
			t.Errorf("accepted credential through %s parser container", tc.format)
		}
	}
}

func TestSettingsPreserveLiteralTemplateSyntax(t *testing.T) {
	for _, value := range []string{"{{ .secrets.OTHER }}", "{{ .workingDir }}", "${NAME}"} {
		body := "developer_instructions = " + string(mustJSON(t, value)) + "\n"
		got, err := NormalizeSettings(Setting{Format: "toml"}, body)
		if err != nil || got["developer_instructions"] != value {
			t.Fatalf("literal preference: %v %v", got, err)
		}
	}
}
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	result, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Previous is the control plane's to fill on a preview; a candidate that
// carries one came from somewhere it should not have.
func TestCandidateRefusesPreviousFile(t *testing.T) {
	d := Definition{Settings: []Setting{{Path: "settings.json", Format: "json"}}}
	file := &File{Path: "settings.json", Content: `{"theme":"light"}`}
	c := Candidate{ID: "c", Baseline: Baseline{HarnessConfigID: "h"}, WriteRevision: 1, Files: []FileChange{{Path: "settings.json", File: file}}}
	if err := d.ValidateCandidate(c); err != nil {
		t.Fatal(err)
	}
	c.Files[0].Previous = &File{Path: "settings.json", Content: `{"theme":"dark"}`}
	if err := d.ValidateCandidate(c); err == nil {
		t.Fatal("candidate carrying a previous file accepted")
	}
}
