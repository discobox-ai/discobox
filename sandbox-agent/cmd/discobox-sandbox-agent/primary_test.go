package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPrimaryGate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux source hooks")
	}
	for _, tc := range []struct {
		name, script string
		mode         os.FileMode
		code         int
	}{
		{"success", "#!/bin/sh\nexit 0\n", 0755, 0},
		{"failure", "#!/bin/sh\nexit 7\n", 0755, 7},
		{"signal", "#!/bin/sh\nkill -TERM $$\n", 0755, 143},
		{"not executable", "#!/bin/sh\nexit 0\n", 0644, 126},
		{"no interpreter", "exit 0\n", 0755, 126},
		{"invalid interpreter", "#!/no/such/interpreter\n", 0755, 126},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			hook := filepath.Join(root, "hook")
			if err := os.WriteFile(hook, []byte(tc.script), tc.mode); err != nil {
				t.Fatal(err)
			}
			code, err := primaryGate(root, hook)
			if code != tc.code || (err == nil) != (tc.code == 0) {
				t.Fatalf("gate = %d, %v; want %d", code, err, tc.code)
			}
		})
	}
}

func TestPrimaryGatePaths(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux source hooks")
	}
	root := t.TempDir()
	hook := filepath.Join(root, "hook")
	if code, err := primaryGate(root, hook); code != 0 || err != nil {
		t.Fatalf("absent hook: %d, %v", code, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, hook); err != nil {
		t.Fatal(err)
	}
	if code, err := primaryGate(root, hook); code != 126 || err == nil {
		t.Fatalf("escaping link: %d, %v", code, err)
	}
	if err := os.Remove(outside); err != nil {
		t.Fatal(err)
	}
	if code, err := primaryGate(root, hook); code != 126 || err == nil {
		t.Fatalf("dangling link: %d, %v", code, err)
	}
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(hook, 0755); err != nil {
		t.Fatal(err)
	}
	if code, err := primaryGate(root, hook); code != 126 || err == nil {
		t.Fatalf("directory: %d, %v", code, err)
	}
}
