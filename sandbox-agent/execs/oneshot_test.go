//go:build !windows

package execs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/x/shorttmp"
)

func oneShotManager(t *testing.T, path string) *Manager {
	t.Helper()
	dir := shorttmp.Dir(t)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         map[string]string{"PATH": path},
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	return manager
}

func writeProgram(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func systemPath(dir string) string {
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func TestRunOnceReturnsWhatTheCommandPrinted(t *testing.T) {
	dir := t.TempDir()
	writeProgram(t, dir, "say", "#!/bin/sh\nprintf 'hello\\n'\n")
	manager := oneShotManager(t, systemPath(dir))

	out, err := manager.RunOnce(context.Background(), OnceRequest{Command: []string{"say"}, MaxOutput: 64})
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("stdout = %q, want hello", out)
	}
}

// A command that will not stop talking is stopped at the limit rather than
// being allowed to fill this process's memory, and what it printed is refused
// rather than half-read.
func TestRunOnceRefusesMoreOutputThanMayBeRead(t *testing.T) {
	dir := t.TempDir()
	writeProgram(t, dir, "shout", "#!/bin/sh\ni=0\nwhile [ $i -lt 200 ]; do printf '0123456789'; i=$((i+1)); done\n")
	manager := oneShotManager(t, systemPath(dir))

	out, err := manager.RunOnce(context.Background(), OnceRequest{Command: []string{"shout"}, MaxOutput: 64})
	if err == nil {
		t.Fatalf("output = %q, want a command printing past the limit refused", out)
	}
	if out != nil {
		t.Fatalf("output = %q, want nothing handed back", out)
	}
}

// What a failing command printed on stderr is available to a caller that has
// somewhere safe to put it, and is not in the error's own message: a one-shot
// runs with the sandbox's environment, and a CLI that cannot authenticate
// prints back what it tried.
func TestRunOnceKeepsStderrOutOfItsMessage(t *testing.T) {
	dir := t.TempDir()
	writeProgram(t, dir, "fail", "#!/bin/sh\necho 'auth failed for key sk-secret-value' >&2\nexit 3\n")
	manager := oneShotManager(t, systemPath(dir))

	_, err := manager.RunOnce(context.Background(), OnceRequest{Command: []string{"fail"}, MaxOutput: 64})
	if err == nil {
		t.Fatal("a failing command succeeded")
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("error = %v, want what it printed kept out of the message", err)
	}
	var failure *OnceFailure
	if !errors.As(err, &failure) {
		t.Fatalf("error = %T, want an *OnceFailure carrying the detail", err)
	}
	if !strings.Contains(failure.Stderr, "sk-secret-value") {
		t.Fatalf("Stderr = %q, want what the command complained about", failure.Stderr)
	}
}

// A command whose children outlive it must not outlive its deadline. Killing
// the child alone leaves whatever it started holding the output open, and the
// wait would not return while anything can still write to it — which, for the
// judge, means a single wedged wrapper and no verdict from that discobox ever
// again.
func TestRunOnceEndsWithItsContextEvenWithChildrenLeftRunning(t *testing.T) {
	dir := t.TempDir()
	writeProgram(t, dir, "wedge", "#!/bin/sh\nsleep 120 &\nsleep 120\n")
	manager := oneShotManager(t, systemPath(dir))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := manager.RunOnce(ctx, OnceRequest{Command: []string{"wedge"}, MaxOutput: 64}); err == nil {
		t.Fatal("a command killed by its deadline reported success")
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("RunOnce() took %s after a 300ms deadline: the wait outlived the command", took)
	}
}

// A name is looked for on the command's own PATH, and a relative entry on it is
// not a place this process may read on the command's behalf: it would be read
// here and run there.
func TestRunOnceResolvesNamesOnTheCommandsOwnPath(t *testing.T) {
	dir := t.TempDir()
	writeProgram(t, dir, "only-here", "#!/bin/sh\nprintf 'found\\n'\n")
	relative := t.TempDir()
	writeProgram(t, relative, "only-here", "#!/bin/sh\nprintf 'wrong\\n'\n")

	manager := oneShotManager(t, systemPath(dir))
	out, err := manager.RunOnce(context.Background(), OnceRequest{Command: []string{"only-here"}, MaxOutput: 64})
	if err != nil || strings.TrimSpace(string(out)) != "found" {
		t.Fatalf("RunOnce() = %q, %v; want the program on the command's PATH", out, err)
	}

	// Not on the PATH at all.
	bare := oneShotManager(t, "/nonexistent-bin")
	if _, err := bare.RunOnce(context.Background(), OnceRequest{Command: []string{"only-here"}, MaxOutput: 64}); err == nil {
		t.Fatal("a name nothing on PATH answers to was run anyway")
	}

	// A relative PATH entry is skipped rather than read against this process's
	// working directory.
	relativeOnly := oneShotManager(t, "."+string(os.PathListSeparator)+"bin")
	if _, err := relativeOnly.RunOnce(context.Background(), OnceRequest{Command: []string{"only-here"}, MaxOutput: 64}); err == nil {
		t.Fatal("a relative PATH entry was searched")
	}
}

func TestRunOnceRefusesAJobItCannotRun(t *testing.T) {
	manager := oneShotManager(t, systemPath(t.TempDir()))
	for _, tc := range []struct {
		name string
		req  OnceRequest
	}{
		{"no command", OnceRequest{MaxOutput: 64}},
		{"a blank command", OnceRequest{Command: []string{"  "}, MaxOutput: 64}},
		{"no bound on the output", OnceRequest{Command: []string{"true"}}},
	} {
		if _, err := manager.RunOnce(context.Background(), tc.req); err == nil {
			t.Fatalf("%s: RunOnce() succeeded", tc.name)
		}
	}
}
