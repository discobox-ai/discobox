package execs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdRunnerKeepsShimRootAndPassesUserToShim(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	systemdRun := filepath.Join(dir, "systemd-run")
	if err := os.WriteFile(systemdRun, []byte("#!/bin/sh\nprintf '%s\n' \"$@\" > "+argsPath+"\n"), 0o755); err != nil {
		t.Fatalf("write fake systemd-run: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	uid := int64(1000)
	gid := int64(1001)
	if _, err := (SystemdRunner{}).Start(context.Background(), StartRequest{
		ID:          "exec-test",
		Unit:        "discobox-exec-test",
		Command:     []string{"true"},
		Workdir:     "/workspace",
		User:        &User{Name: "darren", UID: &uid, GID: &gid},
		SocketPath:  "/run/discobox/harness-terminals/execs/exec-test.sock",
		RuntimePath: "/run/discobox/harness-terminals/execs/exec-test.json",
		LogDir:      "/run/discobox/harness-terminals/execs/logs",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake systemd-run args: %v", err)
	}
	args := string(data)
	if strings.Contains(args, "--uid=") || strings.Contains(args, "--gid=") {
		t.Fatalf("systemd-run args should not set shim uid/gid:\n%s", args)
	}
	if !strings.Contains(args, "exec-shim\n") || !strings.Contains(args, "--user\n") {
		t.Fatalf("systemd-run args did not pass user to exec shim:\n%s", args)
	}
}

// systemctl show exits 0 for a unit systemd has never heard of, reporting it as
// inactive. Only LoadState separates that from a unit that ran and stopped, so
// the parser must carry it through.
func TestUnitStatusFromPropertiesReportsNotFoundUnitUnloaded(t *testing.T) {
	missing := unitStatusFromProperties(map[string]string{
		"Id":          "discobox-exec-ex_gone.service",
		"LoadState":   "not-found",
		"ActiveState": "inactive",
	})
	if missing.Loaded {
		t.Fatalf("not-found unit reported loaded: %#v", missing)
	}
	stopped := unitStatusFromProperties(map[string]string{
		"Id":          "discobox-exec-ex_ran.service",
		"LoadState":   "loaded",
		"ActiveState": "inactive",
	})
	if !stopped.Loaded {
		t.Fatalf("loaded unit reported unloaded: %#v", stopped)
	}
	// An unexpected show output must not read as a vanished unit.
	if !unitStatusFromProperties(map[string]string{"Id": "x", "ActiveState": "active"}).Loaded {
		t.Fatal("unit without LoadState reported unloaded")
	}
}
