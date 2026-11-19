package harness_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/harness/internal/harnesstest"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

func TestCaptureRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the bundled harness capture commands")
	}
	cmd := exec.CommandContext(t.Context(), node, "--test", "runtime/runtime.test.ts")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("capture behavior: %v\n%s", err, output)
	}
}

// Exercise the actual command boundary and the server's validator together:
// self-consistent JavaScript fixtures alone do not prove wire compatibility.
func TestBuiltInCaptureProtocol(t *testing.T) {
	harnesstest.RequireNode(t)
	cases := []struct{ id, folder, credentialPath, credential, settingsPath, settings string }{
		{"pi", "pi", ".pi/agent/auth.json", `{"anthropic":{"type":"oauth","access":"fake-access","refresh":"fake-refresh","expires":1234},"github-copilot":{"type":"oauth","access":"derived","refresh":"fake-github","expires":1234}}`, ".pi/agent/keybindings.json", `{"app.interrupt":["escape"]}`},
		{"omp", "omp", "store.json", `{"version":1,"credentials":[{"provider":"anthropic","credential":{"type":"oauth","access":"fake-access","refresh":"fake-refresh","accountId":"one","expires":1234}},{"provider":"anthropic","credential":{"type":"api_key","key":"fake-key","accountId":"two"}}]}`, ".omp/agent/config.yml", "theme: dark\nmodelRoles:\n  default: sonnet\n"},
		{"opencode", "opencode", "store.json", `[{"id":"local","integrationID":"openai","active":true,"value":{"type":"oauth","access":"fake-access","refresh":"fake-refresh","expires":1234,"methodID":"oauth","metadata":{"accountID":"acct"}}}]`, ".config/opencode/opencode.jsonc", `{"model":"openai/test", /* portable */ "apiKey":"excluded"}`},
		{"copilot", "copilot", ".copilot/config.json", `{"lastLoggedInUser":{"host":"https://github.com","login":"tester"},"authTokens":{"https://github.com:tester":{"token":"fake-token"}}}`, ".copilot/settings.json", `{"model":"test"}`},
		{"claude-code", "claude-code", ".claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":1234,"scopes":["user:profile"],"subscriptionType":"pro"}}`, ".claude/settings.json", `{"model":"test"}`},
		{"codex", "codex-cli", ".codex/auth.json", `{"tokens":{"access_token":"fake-access","refresh_token":"fake-refresh","account_id":"acct"}}`, ".codex/config.toml", "model = \"test\"\n[tui]\nnotifications = true\n"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("the capture command runs in a Linux box")
			}
			staged := harnesstest.Stage(t, tc.folder)
			box := staged.Install(t, nil)
			box.Write(t, tc.credentialPath, tc.credential)
			box.Write(t, tc.settingsPath, tc.settings)
			if tc.id == "omp" || tc.id == "opencode" {
				box.Stub(t, tc.id, "#!/bin/sh\ncat \"$HOME/store.json\"\n")
			}
			var image struct {
				Harness struct {
					SaveBack harnesscapture.Definition `json:"saveBack"`
				} `json:"harness"`
			}
			if err := json.Unmarshal(staged.Label(), &image); err != nil {
				t.Fatal(err)
			}
			definition := image.Harness.SaveBack
			if err := definition.Validate(); err != nil {
				t.Fatal(err)
			}
			// The command the label names is the one installed.
			if definition.ExportCommand[0] != staged.Declaration().CaptureCommand() || staged.Installs("capture.sh") != definition.ExportCommand[0] {
				t.Fatalf("export command %v is not the installed capture command", definition.ExportCommand)
			}
			input, err := json.Marshal(harnesscapture.ExportInput{Version: 1, Definition: definition, Baseline: harnesscapture.Baseline{HarnessConfigID: "config", Files: []harnesscapture.File{}}, Sentinels: map[string]string{}})
			if err != nil {
				t.Fatal(err)
			}
			cmd := box.Command(t, definition.ExportCommand[0], definition.ExportCommand[1:]...)
			cmd.Stdin = bytes.NewReader(input)
			output, err := cmd.Output()
			if err != nil {
				t.Fatal(err)
			}
			var exported harnesscapture.Export
			if err := json.Unmarshal(output, &exported); err != nil {
				t.Fatal(err)
			}
			if len(exported.Credentials) == 0 {
				t.Fatalf("no credentials: %+v", exported.Diagnostics)
			}
			if err := definition.ValidateFiles(exported.Files); err != nil {
				t.Fatalf("exported files: %v", err)
			}
			candidate := harnesscapture.Candidate{ID: "candidate", Baseline: harnesscapture.Baseline{HarnessConfigID: "config"}, WriteRevision: 1, Credentials: exported.Credentials}
			for _, file := range exported.Files {
				candidate.Files = append(candidate.Files, harnesscapture.FileChange{Path: file.Path, File: &file})
			}
			if err := definition.ValidateCandidate(candidate); err != nil {
				t.Fatalf("exported candidate: %v", err)
			}
		})
	}
}
