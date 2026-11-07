package krunvm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPasstStartupFailureKeepsStderr(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "passt")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho 'passt: rejected startup option' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	previous := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = previous }()
	_, err = startPasst(Config{RuntimeDir: dir, PasstPath: executable, PasstSocket: filepath.Join(dir, "passt.sock")})
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("startPasst = %v, want exit status 7", err)
	}
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "passt: rejected startup option") {
		t.Fatalf("launcher stderr = %q, want passt's startup reason", data)
	}
}
