package codexcli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconfigureSeedsExplicitCredentialBindings(t *testing.T) {
	for _, bin := range []string{"node", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	script, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "seed_previous_credential() {")
	if start < 0 {
		t.Fatal("configure seed function missing")
	}
	end := strings.Index(string(script)[start:], "\n}\n") + start + 3
	for _, tc := range []struct{ name, env, content string }{
		{"api-key", "OPENAI_API_KEY", `{"OPENAI_API_KEY":{"$secret":"OPENAI_API_KEY"}}`},
		{"oauth", "CODEX_OAUTH_TOKEN", `{"tokens":{"access_token":{"$secret":"CODEX_OAUTH_TOKEN"},"refresh_token":"discobox-refresh-happens-in-the-control-plane","account_id":"account-one"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			capture := filepath.Join(dir, "harness-capture")
			if err := os.Mkdir(capture, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"runtime.cjs", "settings.cjs"} {
				content, err := os.ReadFile(filepath.Join("..", "capture", name))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(capture, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			previous, err := json.Marshal(map[string]any{"files": []map[string]any{{"path": ".codex/auth.json", "content": tc.content}}})
			if err != nil {
				t.Fatal(err)
			}
			previousPath := filepath.Join(dir, "previous.json")
			authPath := filepath.Join(dir, "auth.json")
			if err := os.WriteFile(previousPath, previous, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "sh", "-c", "set -eu\nclear_captured_credential() { rm -f \"$AUTH_FILE\"; }\n"+string(script)[start:end]+"\nseed_previous_credential\n") //nolint:gosec // Executes the repository-owned seed function in an isolated fixture.
			cmd.Env = append(os.Environ(), "PREVIOUS_ENV="+tc.env, "PREV_"+tc.env+"=sentinel-with-quoted-\"-fixture", "PREVIOUS_CONFIG="+previousPath, "AUTH_PATH=.codex/auth.json", "AUTH_FILE="+authPath, "HARNESS_CAPTURE_DIR="+dir)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("seed: %v %s", err, output)
			}
			raw, err := os.ReadFile(authPath)
			if err != nil {
				t.Fatal(err)
			}
			var auth struct {
				Key    string `json:"OPENAI_API_KEY"`
				Tokens struct {
					Access  string `json:"access_token"`
					Account string `json:"account_id"`
				} `json:"tokens"`
			}
			if err := json.Unmarshal(raw, &auth); err != nil {
				t.Fatal(err)
			}
			got := auth.Key
			if tc.name == "oauth" {
				got = auth.Tokens.Access
				if auth.Tokens.Account != "account-one" {
					t.Fatal("account metadata changed")
				}
			}
			if got != "sentinel-with-quoted-\"-fixture" {
				t.Fatalf("credential was not resolved: %q", got)
			}
		})
	}
}
