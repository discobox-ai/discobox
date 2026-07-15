package claudecode

import (
	"strings"
	"testing"
)

func TestDefinitionConfigure(t *testing.T) {
	def := Driver{}.Definition()
	if def.Configure == nil {
		t.Fatal("Configure = nil, want a configure spec")
	}
	configure := def.Configure

	if len(configure.RunCommand) != 3 || configure.RunCommand[0] != "sh" || configure.RunCommand[1] != "-c" || !strings.Contains(configure.RunCommand[2], `$HOME/.discobox-configure.sh`) {
		t.Fatalf("Configure.RunCommand = %#v, want home-relative configure script", configure.RunCommand)
	}
	if len(configure.InstallCommand) == 0 {
		t.Fatal("Configure.InstallCommand is empty, want the claude-code npm install command")
	}

	if len(configure.Files) != 1 || configure.Files[0].Path != configureScriptPath {
		t.Fatalf("Configure.Files = %#v, want one file at %s", configure.Files, configureScriptPath)
	}
	script := configure.Files[0].Content
	if !strings.Contains(script, "claude ||") {
		t.Fatalf("configure script does not run claude interactively: %s", script)
	}
	if !strings.Contains(script, "/run/discobox/harness-configure.json") {
		t.Fatalf("configure script does not write the harness-configure.json contract: %s", script)
	}
	if !strings.Contains(script, ".claude/.credentials.json") {
		t.Fatalf("configure script does not capture claude credentials: %s", script)
	}
	if !strings.Contains(script, "ANTHROPIC_API_KEY") {
		t.Fatalf("configure script does not fall back to an API key secret: %s", script)
	}
}
