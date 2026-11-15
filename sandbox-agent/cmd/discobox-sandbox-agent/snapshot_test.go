package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// A snapshot is sealed only once every feature is done and none failed, as
// `discobox-provision wait` reads the box's status; a feature turned off is
// done (ADR 26-11-03-986 §1).
func TestASnapshotIsSealedOnlyWhenProvisionedWhole(t *testing.T) {
	for name, tc := range map[string]struct {
		status provisionspec.Status
		want   string
	}{
		"every feature ready or off": {
			status: provisionspec.Status{State: provisionspec.StateReady, Features: map[string]provisionspec.FeatureStatus{
				"core": {State: provisionspec.StateReady}, "brew": {State: provisionspec.StateOff},
			}},
		},
		"a feature failed": {
			status: provisionspec.Status{State: provisionspec.StateFailed, Features: map[string]provisionspec.FeatureStatus{
				"core": {State: provisionspec.StateReady}, "docker": {State: provisionspec.StateFailed, Error: "exit status 1"},
			}},
			want: "the docker feature failed to provision",
		},
		"a feature still provisioning": {
			status: provisionspec.Status{State: provisionspec.StateProvisioning, Features: map[string]provisionspec.FeatureStatus{
				"nix": {State: provisionspec.StateProvisioning},
			}},
			want: "the nix feature is still",
		},
		"provisioning that failed to start": {
			status: provisionspec.Status{State: provisionspec.StateFailed, Error: "load bundle trees"},
			want:   "load bundle trees",
		},
	} {
		path := filepath.Join(t.TempDir(), "provision.json")
		data, err := json.Marshal(tc.status)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		err = requireProvisioned(path)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v, want sealed", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if err := requireProvisioned(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("a box with no provisioning status was sealed")
	}
}

// What a build put under the snapshot's directory goes, and the directory
// stays: on a container it is a bind's mount point.
func TestEmptyDirKeepsTheDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "proxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"snapshot.json", "runtime-config.json", "proxy/client.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := emptyDir(dir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("after emptyDir: %v, %v; want an empty directory", entries, err)
	}
	if err := emptyDir(filepath.Join(dir, "gone")); err != nil {
		t.Errorf("emptyDir of a missing directory: %v", err)
	}
}

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
