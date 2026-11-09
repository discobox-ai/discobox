package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// desktopAppName is the file name the CLI looks for the app under.
func desktopAppName() string {
	if runtime.GOOS == "windows" {
		return "discobox-desktop.exe"
	}
	return "discobox-desktop"
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The app is found by the flag, then the environment, then beside this
// executable, then on PATH (ADR 26-11-07-983 §3), and a CLI that finds none
// says how to build one.
func TestDesktopBinaryOrder(t *testing.T) {
	onPath := t.TempDir()
	writeExecutable(t, filepath.Join(onPath, desktopAppName()))
	t.Setenv("PATH", onPath)
	t.Setenv(desktopBinaryEnv, "")

	got, err := desktopBinary("")
	if err != nil || got != filepath.Join(onPath, desktopAppName()) {
		t.Fatalf("on PATH: got %q, %v", got, err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(self), desktopAppName())
	writeExecutable(t, beside)
	t.Cleanup(func() { _ = os.Remove(beside) })
	if got, err := desktopBinary(""); err != nil || got != beside {
		t.Fatalf("beside the CLI: got %q, %v, want %q", got, err, beside)
	}

	t.Setenv(desktopBinaryEnv, "/from/env")
	if got, _ := desktopBinary(""); got != "/from/env" {
		t.Fatalf("environment: got %q", got)
	}
	if got, _ := desktopBinary("/from/flag"); got != "/from/flag" {
		t.Fatalf("flag: got %q", got)
	}
}

func TestDesktopBinaryNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(desktopBinaryEnv, "")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(self), desktopAppName())); err == nil {
		t.Skip("a desktop app sits beside the test binary")
	}
	_, err = desktopBinary("")
	if err == nil || !strings.Contains(err.Error(), "build:desktop") || !strings.Contains(err.Error(), desktopBinaryEnv) {
		t.Fatalf("got %v, want an error naming build:desktop and %s", err, desktopBinaryEnv)
	}
}

// The app gets no discobox credential or server address from this process's
// environment, and keeps everything else, its own variables included.
func TestDesktopEnvCarriesNoDiscoboxCredentials(t *testing.T) {
	got := desktopEnv([]string{
		"PATH=/bin", "DISPLAY=:0", "DISCOBOX_TOKEN=secret", "DISCOBOX_SERVER=https://x",
		"discobox_token=secret", "DISCOBOX_DESKTOP_DEBUG=1", "DISCOBOX_DESKTOP_BINARY=/app",
	})
	want := []string{"PATH=/bin", "DISPLAY=:0", "DISCOBOX_DESKTOP_DEBUG=1", "DISCOBOX_DESKTOP_BINARY=/app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// What the app logs reaches the terminal with its control sequences escaped,
// a line at a time, however the writes are split.
func TestDesktopStderrIsTerminalSafe(t *testing.T) {
	var out bytes.Buffer
	w := &terminalSafeLines{w: &out}
	for _, chunk := range []string{"bridge ← {\"name\":\"a\x1b]52;c;", "aGk=\x07\"}\nnext\u009b", "2J\ttab"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07\u009b") {
		t.Fatalf("a control reached the terminal: %q", got)
	}
	if !strings.Contains(got, "\n") || !strings.Contains(got, "\t") || strings.Count(got, "\n") != 2 {
		t.Errorf("lines and tabs not kept: %q", got)
	}
}
