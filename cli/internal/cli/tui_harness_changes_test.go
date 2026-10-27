package cli

import (
	"encoding/json"
	"strings"
	"testing"

	api "github.com/discobox-ai/discobox/api/gen"
)

func TestHarnessPreviewEscapesUntrustedDisplayData(t *testing.T) {
	var preview api.HarnessConfigChanges
	if err := json.Unmarshal([]byte(`{"candidateId":"candidate","configRevision":1,"state":"pending","harnessConfigId":"harness","files":[{"path":"settings.json\u001b[2J","file":{"path":"settings.json","content":"{\n  \"nested\": {\"theme\": \"light\"}\n}\u001b[2J"}}],"credentials":[{"provider":"provider","envName":"TOKEN","type":"oauth","account":"account\u001b[2J"}],"diagnostics":[{"code":"warning","path":"settings.json","message":"bad\u001b[2J"}]}`), &preview); err != nil {
		t.Fatal(err)
	}
	got := toTUIHarnessChanges(&preview)
	for _, value := range []string{got.Files[0].Path, got.Files[0].Content, got.Credentials[0], got.Diagnostics[0]} {
		if strings.Contains(value, "\x1b") {
			t.Fatalf("raw terminal control in %q", value)
		}
	}
	if !strings.Contains(got.Files[0].Content, "\n") || !strings.Contains(got.Files[0].Content, `"nested"`) {
		t.Fatalf("nested multiline settings lost: %q", got.Files[0].Content)
	}
	if !strings.Contains(got.Diagnostics[0], "settings.json") {
		t.Fatal("diagnostic path lost")
	}
	if got.CandidateID != "candidate" {
		t.Fatal("candidate identity changed")
	}
}

func TestHarnessChangesAcceptsProjectAndServerFlags(t *testing.T) {
	root := NewRootCommand()
	var output strings.Builder
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--server", "http://127.0.0.1:8080", "--project", "project-review", "admin", "harness", "changes", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "changes SANDBOX") {
		t.Fatalf("wrong command: %s", output.String())
	}
}
