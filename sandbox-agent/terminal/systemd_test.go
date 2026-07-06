package terminal

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/execs"
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
		ID:          "agt-test",
		AgentID:     "claude-code",
		Unit:        "discobox-agent-terminal-agt-test",
		Command:     []string{"claude"},
		Workdir:     "/workspace",
		User:        &execs.User{Name: "darren", UID: &uid, GID: &gid},
		SocketPath:  "/run/discobox/agent-terminals/agt-test.sock",
		RuntimePath: "/run/discobox/agent-terminals/agt-test.json",
		LogDir:      "/run/discobox/agent-terminals/logs",
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
	if !strings.Contains(args, "shim\n") || !strings.Contains(args, "--user\n") {
		t.Fatalf("systemd-run args did not pass user to terminal shim:\n%s", args)
	}
}
