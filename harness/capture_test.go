package harness_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

func TestCaptureRuntime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the bundled harness capture commands")
	}
	cmd := exec.CommandContext(t.Context(), node, "--test", "capture/runtime.test.cjs")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("capture behavior: %v\n%s", err, output)
	}
}

// Exercise the actual command boundary and the server's validator together:
// self-consistent JavaScript fixtures alone do not prove wire compatibility.
func TestBuiltInCaptureProtocol(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for harness capture")
	}
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
			if runtime.GOOS == "windows" && (tc.id == "omp" || tc.id == "opencode") {
				t.Skip("native export fixture requires POSIX executable scripts")
			}
			home := t.TempDir()
			write := func(name, body string, mode os.FileMode) {
				t.Helper()
				p := filepath.Join(home, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(tc.credentialPath, tc.credential, 0o600)
			write(tc.settingsPath, tc.settings, 0o600)
			if tc.id == "omp" || tc.id == "opencode" {
				write("bin/"+tc.id, "#!/bin/sh\ncat \"$HOME/store.json\"\n", 0o700)
			}
			metadata, err := filepath.Abs(filepath.Join(tc.folder, "image.json"))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(metadata)
			if err != nil {
				t.Fatal(err)
			}
			var image struct {
				Harness struct {
					SaveBack harnesscapture.Definition `json:"saveBack"`
				} `json:"harness"`
			}
			if err := json.Unmarshal(raw, &image); err != nil {
				t.Fatal(err)
			}
			definition := image.Harness.SaveBack
			if err := definition.Validate(); err != nil {
				t.Fatal(err)
			}
			input, err := json.Marshal(harnesscapture.ExportInput{Version: 1, Definition: definition, Baseline: harnesscapture.Baseline{HarnessConfigID: "config", Files: []harnesscapture.File{}}, Sentinels: map[string]string{}})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), node, "capture/runtime.cjs", tc.id, metadata, "export") //nolint:gosec // Fixed built-in fixture names and a checkout-owned script.
			cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+filepath.Join(home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
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
