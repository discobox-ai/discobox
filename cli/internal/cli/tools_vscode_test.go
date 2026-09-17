package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/tools"
)

// fakeEditor puts a `code` on PATH that records the arguments it was run with,
// and returns the file it records them to. It is the only way to see what this
// command actually asked the editor for: everything else it does is a file it
// writes, and the launch is the part that has to be right.
func fakeVSCode(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake editor is a shell script")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	// The environment is recorded beside the arguments: whether the WSL prompt
	// was silenced is as much a part of the launch as the flags are.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n" +
		"printf '%s' \"$DONT_PROMPT_WSL_INSTALL\" > " + record + ".env\n"
	path := filepath.Join(dir, "code")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: it has to be executable.
		t.Fatalf("write fake editor: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A DISCOBOX_VSCODE left over in the developer's environment would name a
	// different binary and quietly bypass the one just written.
	t.Setenv("DISCOBOX_VSCODE", "")
	return record
}

// editorEnv is what the fake editor saw in DONT_PROMPT_WSL_INSTALL.
func editorEnv(t *testing.T, record string) string {
	t.Helper()
	data, err := os.ReadFile(record + ".env")
	if err != nil {
		t.Fatalf("the editor was never run: %v", err)
	}
	return string(data)
}

func editorArgs(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the editor was never run: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// runToolsVSCodeCmd runs `tools vscode` against fake with HOME and
// XDG_STATE_HOME redirected, so nothing here touches the real ~/.ssh.
func runToolsVSCodeCmd(t *testing.T, fake *sshConfigFakeServer, args ...string) (home, state, stderr string, err error) {
	t.Helper()
	return runToolsCmd(t, fake, t.TempDir(), "vscode", args...)
}

// runToolsCmd runs `tools <tool>` against fake with home as HOME, the user's
// config directory under it — so the tools a test declares there are the only
// user tools there are — and a fresh XDG_STATE_HOME.
func runToolsCmd(t *testing.T, fake *sshConfigFakeServer, home, tool string, args ...string) (_, state, stderr string, err error) {
	t.Helper()
	state = t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", state)

	server := fake.start(t)
	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "tools", tool}, args...))
	err = cmd.Execute()
	return home, state, errOut.String(), err
}

// declareUserTool writes a tool declaration into home's user tools directory.
func declareUserTool(t *testing.T, home, name, body string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "discobox", "tools")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func vscodeFakeServer() *sshConfigFakeServer {
	return &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox", workdir: "/home/agent/repo"}},
	}
}

// TestToolsVSCodeWritesTheConfigAndOpensTheWorkTree is the whole command: the
// host has to exist in a file ssh reads before the editor is told to use it,
// and the window has to open on the working tree rather than the home directory
// an SSH session lands in.
func TestToolsVSCodeWritesTheConfigAndOpensTheWorkTree(t *testing.T) {
	record := fakeVSCode(t)
	_, state, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}

	configPath, _ := managedPaths(state)
	config := readFile(t, configPath)
	if !strings.Contains(config, "Host devbox ") {
		t.Fatalf("the editor's host is not in the config ssh reads:\n%s", config)
	}
	if !strings.Contains(config, " admin ssh-proxy\n") {
		t.Fatalf("the config does not reach the server through the CLI:\n%s", config)
	}

	// A folder URI rather than --remote and a path: a path argument is what
	// the Windows launcher rewrites into a window on the local WSL directory.
	want := []string{"--new-window", "--folder-uri", "vscode-remote://ssh-remote+devbox/home/agent/repo"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// TestToolsVSCodeSilencesTheWSLInstallPrompt: VS Code's launcher asks whether to
// continue when it finds itself installed inside WSL, and nothing is typing at
// the stdin it asks on — so without this the command hangs, or takes the
// default No and opens nothing.
func TestToolsVSCodeSilencesTheWSLInstallPrompt(t *testing.T) {
	record := fakeVSCode(t)
	if _, _, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	if got := editorEnv(t, record); got != "1" {
		t.Fatalf("DONT_PROMPT_WSL_INSTALL = %q, want \"1\"", got)
	}
}

// A window that reuses the one you are in is a setting, and settings are the
// user's layer: a vscode.yaml of their own replaces the CLI's (ADR 0125).
func TestToolsVSCodeIsReplacedByTheUsersOwnDeclaration(t *testing.T) {
	record := fakeVSCode(t)
	home := t.TempDir()
	declareUserTool(t, home, "vscode.yaml", `runs: host
program: code
args: [--reuse-window, --folder-uri, "vscode-remote://ssh-remote+{ssh.host}{workdir.urlpath}"]
`)
	if _, _, _, err := runToolsCmd(t, vscodeFakeServer(), home, "vscode", "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	want := []string{"--reuse-window", "--folder-uri", "vscode-remote://ssh-remote+devbox/home/agent/repo"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// Arguments past the sandbox belong to the editor, so an extension or a
// --goto reaches it untouched.
func TestToolsVSCodePassesEditorArgumentsThrough(t *testing.T) {
	record := fakeVSCode(t)
	if _, _, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--", "--disable-extensions"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	if args := editorArgs(t, record); args[len(args)-1] != "--disable-extensions" {
		t.Fatalf("editor args = %v, want the passthrough argument last", args)
	}
}

// A sandbox that never said where its source landed still opens, on the
// discobox's root: a connected window whose tree is the box.
func TestToolsVSCodeOpensTheRootWhenNoWorkTreeIsKnown(t *testing.T) {
	record := fakeVSCode(t)
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox"}},
	}
	if _, _, _, err := runToolsVSCodeCmd(t, fake, "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	want := []string{"--new-window", "--folder-uri", "vscode-remote://ssh-remote+devbox/"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// Nothing is written for a window that could never open: a missing editor is
// the one failure the user cannot fix after the fact, so it is found first.
func TestToolsVSCodeFailsBeforeWritingWhenNoEditorIsInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("DISCOBOX_VSCODE", "")
	_, state, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err == nil {
		t.Fatal("expected tools vscode to fail with no editor installed")
	}
	if !strings.Contains(err.Error(), "--program") {
		t.Fatalf("error should say how to name an editor, got: %v", err)
	}
	configPath, _ := managedPaths(state)
	if _, statErr := os.Stat(configPath); statErr == nil {
		t.Fatal("a config was written for a window that never opened")
	}
}

// --program names a build that is not one of the ones looked for.
func TestToolsVSCodeHonorsTheNamedEditor(t *testing.T) {
	record := fakeVSCode(t)
	// The fake is installed as `code`; asking for it by name has to find it
	// rather than fall through to the search.
	if _, _, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--program", "code"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	if args := editorArgs(t, record); len(args) == 0 {
		t.Fatal("the named editor was not run")
	}

	if _, _, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--program", "not-an-editor"); err == nil {
		t.Fatal("expected an editor that is not installed to fail")
	}
}

// builtinTool is the CLI's own declaration of a tool.
func builtinTool(t *testing.T, id string) tools.Definition {
	t.Helper()
	def, ok := tools.Find(builtinTools(), id)
	if !ok || def.Problem != "" {
		t.Fatalf("the CLI declares no runnable %s: %+v", id, def)
	}
	return def
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
