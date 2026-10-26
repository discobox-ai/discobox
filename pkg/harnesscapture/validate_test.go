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
	}{{`{"token":"{{ .secrets.API_KEY }}","account":"me"}`, true}, {`{"token":"real-secret"}`, false}, {`{"token":"{{ .secrets.OTHER }}"}`, false}, {`{"token":"{{ .secrets.API_KEY }}","hidden":"real-secret"}`, false}} {
		err := d.ValidateFiles([]File{{Path: "auth.json", Content: tc.content, Template: true}})
		if (err == nil) != tc.valid {
			t.Fatalf("template acceptance=%v, expected %v", err == nil, tc.valid)
		}
	}
}
func TestPortableSettingsRejectUnknownFields(t *testing.T) {
	for _, s := range []Setting{{Format: "json", Fields: map[string]string{"model": "string"}}, {Format: "toml", Fields: map[string]string{"model": "string"}}, {Format: "yaml", Fields: map[string]string{"model": "string"}}} {
		content := `{"model":"a","apiKey":"secret"}`
		if s.Format == "toml" {
			content = "model = 'a'\napiKey = 'secret'"
		}
		if s.Format == "yaml" {
			content = "model: a\napiKey: secret"
		}
		if _, err := NormalizeSettings(s, content); err == nil {
			t.Fatalf("accepted undeclared field in %s", s.Format)
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
	suffix := "[projects.{{ .workingDir | json }}]\ntrust_level = \"trusted\"\n"
	d := Definition{Settings: []Setting{{Path: ".codex/config.toml", Format: "toml", Fields: map[string]string{"model": "string"}, TemplateSuffix: suffix}}}
	f := File{Path: ".codex/config.toml", Content: "model = \"next\"\n" + suffix, Template: true}
	if err := d.ValidateFiles([]File{f}); err != nil {
		t.Fatal(err)
	}
	f.Content = "model = \"next\"\n[projects.\"/private/workspace\"]\ntrust_level = \"trusted\"\n"
	if err := d.ValidateFiles([]File{f}); err == nil {
		t.Fatal("accepted captured workspace trust")
	}
}
