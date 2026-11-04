package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExporterProcess(_ *testing.T) {
	if os.Getenv("DISCOBOX_DOCS_TEST_PROCESS") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"discobox-dev"}, os.Args[i+1:]...)
				break
			}
		}
		flag.CommandLine = flag.NewFlagSet("discobox-docs", flag.ExitOnError)
		main()
		os.Exit(0)
	}
}

func TestExportDeterminismAndCheck(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A local tool must never become a public command or contribute its help.
	config := filepath.Join(dir, "config")
	if err := os.MkdirAll(filepath.Join(config, "discobox", "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "discobox", "tools", "private.yaml"), []byte("name: private\ndescription: private description must not leak\nprogram: secret-program\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(extra []string, args ...string) ([]byte, error) {
		//nolint:gosec // Only this test executable is run, with test-owned arguments.
		cmd := exec.CommandContext(t.Context(), exe, append([]string{"-test.run=^TestExporterProcess$", "--"}, args...)...)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "DISCOBOX_") && !strings.HasPrefix(entry, "XDG_CONFIG_HOME=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "DISCOBOX_DOCS_TEST_PROCESS=1")
		cmd.Env = append(cmd.Env, extra...)
		return cmd.CombinedOutput()
	}
	first := filepath.Join(dir, "one.md")
	second := filepath.Join(dir, "two.md")
	if out, err := run(nil, "-output", first); err != nil {
		t.Fatalf("generate: %s %v", out, err)
	}
	dirty := []string{"DISCOBOX_TOKEN=credential-canary", "DISCOBOX_PROJECT=private-project", "DISCOBOX_SERVER=http://invalid.invalid", "DISCOBOX_LEADER=invalid", "XDG_CONFIG_HOME=" + config}
	if out, err := run(dirty, "-output", second); err != nil {
		t.Fatalf("generate with overrides: %s %v", out, err)
	}
	a, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("environment or user declarations changed the export")
	}
	for _, s := range []string{dir, "credential-canary", "private-project", "private description", "discobox-dev"} {
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("export contains %q", s)
		}
	}
	if out, err := run(dirty, "-check", "-output", first); err != nil {
		t.Fatalf("current check: %s %v", out, err)
	}
	if err := os.WriteFile(first, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(nil, "-check", "-output", first); err == nil || !bytes.Contains(out, []byte("is stale")) {
		t.Fatalf("stale check: %s %v", out, err)
	}
	unchanged, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != "stale\n" {
		t.Fatal("check rewrote stale output")
	}
	if out, err := run(nil, "-check", "-output", filepath.Join(dir, "missing.md")); err == nil {
		t.Fatalf("missing file check succeeded: %s", out)
	}
	if out, err := run(nil, "unexpected"); err == nil {
		t.Fatalf("unexpected argument accepted: %s", out)
	}
}
