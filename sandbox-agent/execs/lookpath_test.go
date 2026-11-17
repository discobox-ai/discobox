package execs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A command is looked up on the PATH its own environment ends up with: the
// last entry, as os/exec hands the child, not this process's.
func TestLookPathUsesTheCommandsPath(t *testing.T) {
	name := "only-here"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + t.TempDir(), "PATH=" + bin}
	got, err := lookPath("only-here", env)
	if err != nil {
		t.Fatalf("lookPath: %v", err)
	}
	if got != filepath.Join(bin, name) {
		t.Fatalf("lookPath = %q, want %q", got, filepath.Join(bin, name))
	}
	if _, err := lookPath("only-here", []string{"PATH=" + t.TempDir()}); err == nil {
		t.Fatal("found a command that is not on the command's PATH")
	}
}

// Windows' names ignore case, so an exec that sets Path sets the PATH its
// command runs with; anywhere else they are two variables.
func TestEnvValueComparesNamesAsThePlatformDoes(t *testing.T) {
	got := envValue([]string{"PATH=/agent", "Path=/exec"}, "PATH")
	want := "/agent"
	if runtime.GOOS == "windows" {
		want = "/exec"
	}
	if got != want {
		t.Fatalf("envValue = %q, want %q", got, want)
	}
}
