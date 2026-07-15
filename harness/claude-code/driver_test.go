package claudecode

import (
	"os"
	"strings"
	"testing"
)

func TestDefinitionConfigure(t *testing.T) {
	def := Driver{}.Definition()
	if def.Configure == nil {
		t.Fatal("Configure = nil, want a configure spec")
	}
	scriptBytes, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	if !strings.Contains(script, "claude ||") {
		t.Fatalf("configure script does not run claude interactively: %s", script)
	}
	if !strings.Contains(script, "/run/discobox/harness-configure.json") {
		t.Fatalf("configure script does not write the harness-configure.json contract: %s", script)
	}
	if !strings.Contains(script, ".claude/.credentials.json") {
		t.Fatalf("configure script does not capture claude credentials: %s", script)
	}
	if strings.Contains(script, "files.push({ path: '.claude/.credentials.json'") {
		t.Fatalf("configure script exposes credentials as a public harness file: %s", script)
	}
	if !strings.Contains(script, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(script, "oauth.accessToken") {
		t.Fatalf("configure script does not convert Claude credentials into an OAuth secret: %s", script)
	}
	if !strings.Contains(script, "ANTHROPIC_API_KEY") {
		t.Fatalf("configure script does not fall back to an API key secret: %s", script)
	}
}
