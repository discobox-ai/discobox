package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Mount namespaces, and nsenter that enters one, are Linux's alone: the
// snapshot command runs in a Linux box, and these stand in a shell script
// for nsenter.

// nsLinks makes the two /proc mount-namespace links the snapshot command
// compares, naming self and init.
func nsLinks(t *testing.T, self, init string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	selfNS, initNS := filepath.Join(dir, "self"), filepath.Join(dir, "init")
	if err := os.Symlink(self, selfNS); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(init, initNS); err != nil {
		t.Fatal(err)
	}
	return selfNS, initNS
}

// A taker's exec in a mount namespace of its own, as boxd runs every exec, is
// run again in systemd's, so the tmpfs it mounts over the snapshot's
// directory, and the bridge config it writes there, are the ones the units
// it starts see (#1197). One already there, or in a box whose init is not
// systemd, runs where it is.
func TestTheSnapshotCommandRunsInSystemdsMountNamespace(t *testing.T) {
	bin := t.TempDir()
	nsenter := filepath.Join(bin, "nsenter")
	if err := os.WriteFile(nsenter, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx := context.Background()

	selfNS, initNS := nsLinks(t, "mnt:[4026532075]", "mnt:[4026532073]")
	cmd, err := inInitMountNamespace(ctx, true, selfNS, initNS)
	if err != nil || cmd == nil {
		t.Fatalf("a private namespace: %v, %v; want the command run again", cmd, err)
	}
	agent, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{nsenter, "--target", "1", "--mount", "--", agent, "snapshot"}; !slices.Equal(cmd.Args, want) {
		t.Errorf("runs %q, want %q", cmd.Args, want)
	}

	if cmd, err := inInitMountNamespace(ctx, false, selfNS, initNS); err != nil || cmd != nil {
		t.Errorf("init is not systemd: %v, %v; want it run where it is", cmd, err)
	}
	sameSelf, sameInit := nsLinks(t, "mnt:[4026532073]", "mnt:[4026532073]")
	if cmd, err := inInitMountNamespace(ctx, true, sameSelf, sameInit); err != nil || cmd != nil {
		t.Errorf("systemd's namespace already: %v, %v; want it run where it is", cmd, err)
	}

	t.Setenv("PATH", t.TempDir())
	if _, err := inInitMountNamespace(ctx, true, selfNS, initNS); err == nil || !strings.Contains(err.Error(), "no nsenter") {
		t.Errorf("no nsenter: %v; want it refused, saying so", err)
	}
	if _, err := inInitMountNamespace(ctx, true, filepath.Join(t.TempDir(), "missing"), initNS); err == nil {
		t.Error("an unreadable namespace was taken for systemd's")
	}
}

// The command run again is the build's result: its exit status, with the
// plan on its standard input.
func TestTheSnapshotCommandRunAgainIsTheResult(t *testing.T) {
	bin := t.TempDir()
	nsenter := filepath.Join(bin, "nsenter")
	// The stand-in reads the plan and exits with the status it names.
	if err := os.WriteFile(nsenter, []byte("#!/bin/sh\nread status\nexit \"$status\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{0, 1, 3} {
		plan := filepath.Join(t.TempDir(), "plan")
		if err := os.WriteFile(plan, []byte(strconv.Itoa(status)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdin, err := os.Open(plan)
		if err != nil {
			t.Fatal(err)
		}
		cmd := enterCommand(context.Background(), nsenter, "/agent")
		cmd.Stdin = stdin
		if got := runEntered(cmd); got != status {
			t.Errorf("exit %d was reported as %d", status, got)
		}
		_ = stdin.Close()
	}
}
