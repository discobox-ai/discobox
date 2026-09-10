package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeZed puts a `zed` on PATH that records the arguments it was run with, and
// returns the file it records them to. Like fakeVSCode, it is the only way to
// see what this command actually asked the editor for: everything else it does
// is a file it writes, and the launch is the part that has to be right.
func fakeZed(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake editor is a shell script")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\n"
	if err := os.WriteFile(filepath.Join(dir, "zed"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: it has to be executable.
		t.Fatalf("write fake editor: %v", err)
	}
	// PATH is replaced rather than prepended to. The unambiguous names are
	// looked for first, so a real `zeditor` anywhere on the developer's own
	// PATH would beat the `zed` just written and open their editor.
	t.Setenv("PATH", dir)
	// A DISCOBOX_ZED left over in the developer's environment would name a
	// different binary and quietly bypass the one just written.
	t.Setenv(zedEditorEnv, "")
	return record
}

// runToolsZedCmd runs `tools zed` against fake with HOME and XDG_STATE_HOME
// redirected, so nothing here touches the real ~/.ssh.
func runToolsZedCmd(t *testing.T, fake *sshConfigFakeServer, args ...string) (home, state, stderr string, err error) {
	t.Helper()
	home, state = t.TempDir(), t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_STATE_HOME", state)

	server := fake.start(t)
	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "tools", "zed"}, args...))
	err = cmd.Execute()
	return home, state, errOut.String(), err
}

// TestToolsZedWritesTheConfigAndOpensTheWorkTree is the whole command: Zed
// shells out to the ssh on PATH, so the host has to exist in the file that ssh
// reads before Zed is told to use it, and the window has to open on the working
// tree rather than the home directory an SSH session lands in.
func TestToolsZedWritesTheConfigAndOpensTheWorkTree(t *testing.T) {
	record := fakeZed(t)
	_, state, _, err := runToolsZedCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err != nil {
		t.Fatalf("execute tools zed: %v", err)
	}

	configPath, _ := managedPaths(state)
	config := readFile(t, configPath)
	if !strings.Contains(config, "Host devbox ") {
		t.Fatalf("the editor's host is not in the config ssh reads:\n%s", config)
	}
	if !strings.Contains(config, " admin ssh-proxy\n") {
		t.Fatalf("the config does not reach the server through the CLI:\n%s", config)
	}

	// A URL rather than a path argument, on every platform: the launcher passes
	// a known scheme through untouched and rewrites a bare path. No user and no
	// port either — the stanza carries both.
	want := []string{"--new", "ssh://devbox/home/agent/repo"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// The window opens beside whatever you were already editing, not over it.
func TestToolsZedReusesTheWindowOnlyWhenAsked(t *testing.T) {
	record := fakeZed(t)
	if _, _, _, err := runToolsZedCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--reuse-window"); err != nil {
		t.Fatalf("execute tools zed: %v", err)
	}
	args := editorArgs(t, record)
	if !contains(args, "--reuse") || contains(args, "--new") {
		t.Fatalf("editor args = %v, want --reuse alone", args)
	}
}

// Arguments past the sandbox belong to the editor.
func TestToolsZedPassesEditorArgumentsThrough(t *testing.T) {
	record := fakeZed(t)
	if _, _, _, err := runToolsZedCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--", "--foreground"); err != nil {
		t.Fatalf("execute tools zed: %v", err)
	}
	if args := editorArgs(t, record); args[len(args)-1] != "--foreground" {
		t.Fatalf("editor args = %v, want the passthrough argument last", args)
	}
}

// A workdir with a space in it survives being part of a URL: Zed parses the URL
// and percent-decodes the path, so it has to arrive encoded.
func TestToolsZedEncodesTheWorkdir(t *testing.T) {
	record := fakeZed(t)
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox", workdir: "/home/agent/my repo"}},
	}
	if _, _, _, err := runToolsZedCmd(t, fake, "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools zed: %v", err)
	}
	want := []string{"--new", "ssh://devbox/home/agent/my%20repo"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// A sandbox that never said where its source landed still opens. Zed's URL has
// no way to say "connected, nothing open" the way VS Code's --remote does, so
// it opens on the discobox's root: a connected window whose tree is the box.
func TestToolsZedOpensTheRootWhenNoWorkTreeIsKnown(t *testing.T) {
	record := fakeZed(t)
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox"}},
	}
	if _, _, _, err := runToolsZedCmd(t, fake, "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools zed: %v", err)
	}
	want := []string{"--new", "ssh://devbox/"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// Nothing is written for a window that could never open: a missing editor is
// the one failure the user cannot fix after the fact, so it is found first.
func TestToolsZedFailsBeforeWritingWhenNoEditorIsInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(zedEditorEnv, "")
	_, state, _, err := runToolsZedCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err == nil {
		t.Fatal("expected tools zed to fail with no editor installed")
	}
	for _, want := range []string{"Zed", "--editor"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name the editor and how to point at one, got: %v", err)
		}
	}
	configPath, _ := managedPaths(state)
	if _, statErr := os.Stat(configPath); statErr == nil {
		t.Fatal("a config was written for a window that never opened")
	}
}

// --editor names a build that is not one of the ones looked for, which is how a
// Zed installed under a packager's own name is reached.
func TestToolsZedHonorsTheNamedEditor(t *testing.T) {
	record := fakeZed(t)
	if _, _, _, err := runToolsZedCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--editor", "zed"); err != nil {
		t.Fatalf("execute tools zed: %v", err)
	}
	if args := editorArgs(t, record); len(args) == 0 {
		t.Fatal("the named editor was not run")
	}

	if _, _, _, err := runToolsZedCmd(t, vscodeFakeServer(),
		"--discobox-id", "sbx_devbox00000001", "--editor", "not-an-editor"); err == nil {
		t.Fatal("expected an editor that is not installed to fail")
	}
}

// The two editor commands do not share a binary, and neither may pick up the
// other's: a machine with only VS Code installed has no Zed, and says so.
//
// PATH is narrowed to the fake VS Code alone, the way the no-editor test
// narrows it to an empty directory. Inherited, a developer's real Zed would be
// on it — and this is the one test whose subject is an editor *not* being
// found, so finding one would not merely fail: it would open that editor on a
// host that exists only in the temporary HOME this test just pointed at.
func TestToolsZedDoesNotFallBackToVSCode(t *testing.T) {
	record := fakeVSCode(t)
	t.Setenv("PATH", filepath.Dir(record))
	t.Setenv(zedEditorEnv, "")
	if _, _, _, err := runToolsZedCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001"); err == nil {
		t.Fatal("expected tools zed to fail on a machine with only VS Code installed")
	}
}
