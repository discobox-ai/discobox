package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A redirect is not a terminal: nothing was ever set on it, and escape
// sequences written there are bytes in somebody's capture.
func TestResetTerminalStaysOutOfARedirect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer file.Close()

	NewOSConsole(os.Stdin, file).ResetTerminal()

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(written) > 0 {
		t.Fatalf("wrote %q to a redirect, want nothing", written)
	}
}

// Stdout that is not a file at all — an in-memory writer — is no obstacle: the
// session still owns the terminal's mode, it just has nowhere to write.
func TestResetTerminalWithoutAnOutputFile(t *testing.T) {
	console := NewOSConsole(os.Stdin, &strings.Builder{})
	if console == nil {
		t.Fatal("a console over stdin should exist whatever stdout is")
	}
	console.ResetTerminal()
}
