package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	discoboxharness "github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
)

// claudeImageHarness reads the Claude harness's authoring-time image.json.
func claudeImageHarness(t *testing.T) config.Harness {
	t.Helper()
	return imageHarness(t, "claude-code")
}

// imageHarness reads a harness's authoring-time image.json directly (the
// runtime carrier is the OCI label now, but the file itself is still the
// build-time source of truth — see harness/DESIGN.md) and converts its harness
// contract to config.Harness, the same shape sandbox.json decodes.
func imageHarness(t *testing.T, dir string) config.Harness {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "harness", dir, "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata discoboxharness.ImageMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Harness == nil {
		t.Fatalf("%s image.json has no harness contract", dir)
	}
	files := make([]config.HarnessFile, 0, len(metadata.Harness.Files))
	for _, file := range metadata.Harness.Files {
		files = append(files, config.HarnessFile{
			Path: file.Path, Content: file.Content, CreateOnly: file.CreateOnly, Template: file.Template,
		})
	}
	return config.Harness{
		ID:      metadata.Harness.ID,
		Name:    metadata.Harness.Name,
		Command: metadata.Harness.RunCommand,
		Files:   files,
	}
}

func TestFileInstallerRendersSandboxConfigTemplate(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		WorkingDir:    `/workspace/project"quoted`,
		SandboxConfig: map[string]any{
			"sources": []any{
				map[string]any{"slug": "primary", "target": `/workspace/project"quoted`},
				map[string]any{"slug": "extra", "target": "/workspace/extra"},
			},
		},
	}
	harness := claudeImageHarness(t)

	if err := installer.EnsureInstalled(context.Background(), harness, "", nil); err != nil {
		t.Fatalf("install files: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse rendered config: %v\n%s", err, data)
	}
	if !state.Projects[`/workspace/project"quoted`].Trusted {
		t.Fatalf("projects = %#v, want safely encoded trusted project", state.Projects)
	}
	if len(state.Projects) != 1 {
		t.Fatalf("projects = %#v, want only the working directory trusted", state.Projects)
	}
}

func TestFileInstallerDoesNotRenderLiteralFile(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		SandboxConfig: map[string]any{"name": "rendered"},
	}
	harness := config.Harness{ID: "literal", Files: []config.HarnessFile{{Path: "config", Content: `{{ .name }}`}}}
	if err := installer.EnsureInstalled(context.Background(), harness, "", nil); err != nil {
		t.Fatalf("install files: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{{ .name }}` {
		t.Fatalf("literal content = %q", data)
	}
}

// A sandbox with no source -- `discobox run` with nothing to clone, and every
// configure sandbox -- still has a directory its harness is launched in, and
// that is the directory the trust rendering follows. Keying it off the primary
// source instead left these sandboxes trusting nothing, so Claude Code opened
// on the trust dialog for /workspace instead of on the work.
//
// This goes through FileInstaller rather than calling the renderer directly:
// the context a template is guaranteed is the one templateContext builds, and a
// bare map is not that -- `.secrets` and `.workingDir` are always present there,
// so a template may ask whether a credential exists and where the harness runs.
func TestClaudeTemplateTrustsWorkingDirectoryWithoutAnySource(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home, WorkingDir: "/workspace", SandboxConfig: map[string]any{}}
	if err := installer.EnsureInstalled(context.Background(), claudeImageHarness(t), "", nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		PrimaryAPIKey any `json:"primaryApiKey"`
		Projects      map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rendered, &state); err != nil {
		t.Fatalf("parse Claude config: %v\n%s", err, rendered)
	}
	if !state.Projects["/workspace"].Trusted {
		t.Fatalf("projects = %#v, want the source-less sandbox's working directory trusted", state.Projects)
	}
	// No secret bound either: the key must be absent, not present and empty.
	if state.PrimaryAPIKey != nil {
		t.Fatalf("source-less Claude config unexpectedly has primaryApiKey: %#v", state)
	}
}

// Codex's config.toml is captured by its configure flow and persisted, so a
// copy of this template outlives the image that wrote it and can be delivered
// to a sandbox whose agent predates `workingDir`. Under missingkey=zero the
// bare form renders `[projects.null]` -- valid TOML that silently trusts a
// project named null -- so the stanza is guarded and degrades to no trust at
// all, which is the older and visible failure.
//
// The absent key that older agent would produce cannot be built here, since
// templateContext always sets it; the guard treats absent and empty alike, so
// an empty WorkingDir is what pins the behavior.
func TestCodexTemplateRendersNoTrustRatherThanANullProject(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home, SandboxConfig: map[string]any{}}
	if err := installer.EnsureInstalled(context.Background(), imageHarness(t, "codex-cli"), "", nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "projects.") {
		t.Fatalf("rendered config.toml = %s, want no [projects] table at all", rendered)
	}
}

// Codex gates the same way on its own trust screen, from the same context key.
func TestCodexTemplateTrustsWorkingDirectoryWithoutAnySource(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home, WorkingDir: "/workspace", SandboxConfig: map[string]any{}}
	if err := installer.EnsureInstalled(context.Background(), imageHarness(t, "codex-cli"), "", nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "[projects.\"/workspace\"]\ntrust_level = \"trusted\""; !strings.Contains(string(rendered), want) {
		t.Fatalf("rendered config.toml = %s, want %s", rendered, want)
	}
}

// A harness that authenticates from a file needs its sentinel placed in that
// file, which is what `.secrets` is for. It carries a sentinel, not a
// credential — the proxy swaps it outbound — so it is safe to render into a
// file the way an env var was safe to export.
func TestFileInstallerRendersSentinelsIntoATemplate(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		SandboxConfig: map[string]any{"sources": []any{}},
		Secrets: func() map[string]string {
			//nolint:gosec // A sentinel, which is non-secret by construction.
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-sentinel"}
		},
	}
	harness := config.Harness{
		ID: "claude", Name: "Claude",
		Files: []config.HarnessFile{{
			Path:     ".claude/.credentials.json",
			Template: true,
			Content:  `{"accessToken":"{{ index .secrets "CLAUDE_CODE_OAUTH_TOKEN" }}"}`,
		}},
	}
	if err := installer.EnsureInstalled(context.Background(), harness, "", map[string]string{"HOME": home}); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := `{"accessToken":"sk-ant-oat01-sentinel"}`; string(got) != want {
		t.Fatalf("rendered = %s, want %s", got, want)
	}
}

// The sentinel map is added to a copy. Mutating the shared sandbox config would
// leak one install's secrets into every later render, including a harness that
// never asked for them.
func TestFileInstallerTemplateContextDoesNotMutateSandboxConfig(t *testing.T) {
	shared := map[string]any{"sources": []any{}}
	installer := FileInstaller{
		SandboxConfig: shared,
		Secrets:       func() map[string]string { return map[string]string{"A": "b"} },
	}
	ctx := installer.templateContext()
	if _, ok := ctx["secrets"]; !ok {
		t.Fatal("template context carries no secrets")
	}
	if _, ok := ctx["workingDir"]; !ok {
		t.Fatal("template context carries no working directory")
	}
	for _, key := range []string{"secrets", "workingDir"} {
		if _, ok := shared[key]; ok {
			t.Fatalf("sandbox config was mutated by building the template context: %s", key)
		}
	}
}

// The credentials template the claude-code configure flow emits, byte for byte
// as its write_output builds it. It is JSON-encoded to become the file's
// content, so any quote inside a template action arrives backslash-escaped and
// the parser rejects it with `unexpected "\\" in operand` — a break that only
// shows up when a sandbox tries to launch, long after configure reported
// success. Dotted field access avoids the quotes entirely.
func TestConfiguredCredentialsTemplateParsesAndRenders(t *testing.T) {
	emitted, err := json.Marshal(map[string]any{
		//nolint:gosec // Template actions and placeholders, not credentials.
		"claudeAiOauth": map[string]any{
			"accessToken":      "{{ .secrets.CLAUDE_CODE_OAUTH_TOKEN }}",
			"refreshToken":     "discobox-refresh-happens-in-the-control-plane",
			"expiresAt":        int64(4102444800000),
			"scopes":           []string{"user:inference", "user:profile"},
			"subscriptionType": "max",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(emitted, []byte(`\"`)) {
		t.Fatalf("emitted template carries escaped quotes, which the parser rejects: %s", emitted)
	}

	home := t.TempDir()
	installer := FileInstaller{
		HomeDirectory: home,
		SandboxConfig: map[string]any{},
		Secrets: func() map[string]string {
			//nolint:gosec // A sentinel, which is non-secret by construction.
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-sentinel"}
		},
	}
	harness := config.Harness{
		ID: "claude", Name: "Claude",
		Files: []config.HarnessFile{{
			Path: ".claude/.credentials.json", Template: true, Content: string(emitted),
		}},
	}
	if err := installer.EnsureInstalled(context.Background(), harness, "", map[string]string{"HOME": home}); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Rendered output must still be the JSON Claude Code reads, with the
	// sentinel in place of the action.
	raw, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rendered struct {
		ClaudeAiOauth struct {
			AccessToken string   `json:"accessToken"`
			ExpiresAt   int64    `json:"expiresAt"`
			Scopes      []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &rendered); err != nil {
		t.Fatalf("rendered file is not valid JSON: %v\n%s", err, raw)
	}
	if rendered.ClaudeAiOauth.AccessToken != "sk-ant-oat01-sentinel" {
		t.Fatalf("accessToken = %q, want the sandbox's sentinel", rendered.ClaudeAiOauth.AccessToken)
	}
	// Remote Control is gated on finding this scope recorded beside the token.
	if !slices.Contains(rendered.ClaudeAiOauth.Scopes, "user:profile") {
		t.Fatalf("scopes = %v, want the captured set to survive rendering", rendered.ClaudeAiOauth.Scopes)
	}
}

// The console-account credential goes back where Claude Code reads it from:
// `primaryApiKey` in ~/.claude.json, the file the login itself wrote it to. The
// template is image-owned because, unlike the subscription credential, there is
// no captured metadata to replay -- only the sentinel.
func TestClaudeTemplateRendersPrimaryApiKeyFromSentinel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		secrets map[string]string
		wantKey string
	}{
		{
			name:    "console account login",
			secrets: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-sentinel"},
			wantKey: "sk-ant-sentinel",
		},
		{
			// A subscription login binds the other variable; asking for a key
			// that is not there must render a valid file without one, not fail.
			name:    "subscription login only",
			secrets: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oat-sentinel"},
		},
		{
			name:    "no secrets at all",
			secrets: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			installer := FileInstaller{
				HomeDirectory: home,
				WorkingDir:    "/workspace/app",
				SandboxConfig: map[string]any{},
				Secrets:       func() map[string]string { return tc.secrets },
			}
			if err := installer.EnsureInstalled(context.Background(), claudeImageHarness(t), "", nil); err != nil {
				t.Fatalf("install: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			var state struct {
				PrimaryAPIKey string `json:"primaryApiKey"`
				Projects      map[string]struct {
					Trusted bool `json:"hasTrustDialogAccepted"`
				} `json:"projects"`
			}
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatalf("rendered .claude.json is not valid JSON: %v\n%s", err, raw)
			}
			if state.PrimaryAPIKey != tc.wantKey {
				t.Fatalf("primaryApiKey = %q, want %q", state.PrimaryAPIKey, tc.wantKey)
			}
			// Adding the key must not cost the trust rendering that shares the file.
			if !state.Projects["/workspace/app"].Trusted {
				t.Fatalf("projects = %#v, want the working directory still trusted", state.Projects)
			}
		})
	}
}
