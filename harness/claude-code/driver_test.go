package claudecode

import (
	"os"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/harness"
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
	if !strings.Contains(script, harness.ConfigureOutputPath) {
		t.Fatalf("configure script does not write the harness-configure.json contract: %s", script)
	}
	if !strings.Contains(script, harness.ConfigurePreviousConfigPath) {
		t.Fatalf("configure script ignores the seeded previous configuration: %s", script)
	}
	// Reuse goes through the PREV_ sentinel and comes back as usePrevious, so no
	// credential is read or re-emitted when the existing one is kept.
	if !strings.Contains(script, harness.ConfigurePreviousEnvPrefix+"$PREVIOUS_ENV") {
		t.Fatalf("configure script does not read the PREV_ sentinel: %s", script)
	}
	if !strings.Contains(script, "usePrevious") {
		t.Fatalf("configure script cannot report keeping the previous secret: %s", script)
	}
	if !strings.Contains(script, "claude setup-token") {
		t.Fatalf("configure script offers no web login: %s", script)
	}
	if !strings.Contains(script, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(script, "ANTHROPIC_API_KEY") {
		t.Fatalf("configure script does not offer both auth secrets: %s", script)
	}
	if !strings.Contains(script, "claude -p") {
		t.Fatalf("configure script does not verify the credential with a test prompt: %s", script)
	}
	// Credentials are secrets, never public harness files: the flow returns the
	// credential as a secret and leaves the non-secret files to the image.
	if strings.Contains(script, "files.push(") || strings.Contains(script, ".credentials.json'") {
		t.Fatalf("configure script exposes credential state as a public harness file: %s", script)
	}
}
