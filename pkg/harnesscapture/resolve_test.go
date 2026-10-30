package harnesscapture

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCredentialBindingsAreJSONData(t *testing.T) {
	d := Definition{Providers: []Provider{{ID: "vendor", EnvName: "KEY", Types: []string{"token"}}}, CredentialFiles: []string{"auth.json"}, CredentialSettings: []Setting{{Path: "auth.json", Fields: map[string]string{"token": "secret", "account": "string"}}}}
	const content = `{"token":{"$secret":"KEY"},"account":"{{ literal }}"}`
	sentinel := "sentinel\"\\\nwith escapes"
	got, err := d.ResolveCredential("auth.json", content, map[string]string{"KEY": sentinel})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["token"] != sentinel || doc["account"] != "{{ literal }}" {
		t.Fatalf("incorrect JSON: %#v", doc)
	}
	literal, err := d.ResolveCredential("settings.json", content, nil)
	if err != nil || literal != content {
		t.Fatal("ordinary settings were interpreted")
	}
	if _, err := d.ResolveCredential("auth.json", content, nil); err == nil {
		t.Fatal("missing binding installed an empty credential")
	}
	if _, err := d.ResolveCredential("auth.json", `{"token":{"$secret":"OTHER"}}`, map[string]string{"OTHER": "other"}); err == nil {
		t.Fatal("unregistered binding accepted")
	}
	if err := d.ValidateFiles([]File{{Path: "auth.json", Content: `{"token":{"$secret":"KEY","extra":"raw"}}`}}); err == nil {
		t.Fatal("reference with extra fields accepted")
	}
}

func TestLegacyDeliveryUpgradeNeverExecutesTemplates(t *testing.T) {
	for _, suffix := range legacyCodexTrust {
		old, err := json.Marshal([]map[string]any{{"path": ".codex/config.toml", "template": true, "content": "model = \"saved\"\n" + suffix + "\n"}, {"path": "auth.json", "template": true, "content": `{"token":"{{ .secrets.KEY }}"}`}})
		if err != nil {
			t.Fatal(err)
		}
		next, err := UpgradeLegacyJSON(old)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(next), "{{") || strings.Contains(string(next), `"template"`) {
			t.Fatalf("legacy syntax survived: %s", next)
		}
		again, err := UpgradeLegacyJSON(next)
		if err != nil || string(again) != string(next) {
			t.Fatal("upgrade is not idempotent")
		}
	}
	raw := []byte(`{"files":[{"path":"custom","template":true,"content":"{{ .name }}"}]}`)
	if _, err := UpgradeLegacyJSON(raw); err == nil {
		t.Fatal("arbitrary template silently changed or executed")
	}
	data := []byte(`{"sequence":18446744073709551615,"templateSuffix":"retired","items":[null,1]}`)
	next, err := UpgradeLegacyJSON(data)
	if err != nil || !strings.Contains(string(next), "18446744073709551615") || !strings.Contains(string(next), "[null,1]") {
		t.Fatalf("durable data changed: %s %v", next, err)
	}
}
