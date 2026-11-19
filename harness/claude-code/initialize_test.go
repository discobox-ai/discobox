package claudecode

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness/internal/harnesstest"
)

// The launcher prepares local trust and onboarding under the lock the
// agent's handoff holds, so it never restores a raw login over a sentinel the
// handoff just installed.
func TestInitializationWaitsForCredentialHandoffLock(t *testing.T) {
	for _, bin := range []string{"node", "flock", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "discobox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, ".claude.json")
	const initial = `{"primaryApiKey":"newer-local-fixture"}`
	if err := os.WriteFile(state, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	hold := exec.CommandContext(t.Context(), "flock", "-x", filepath.Join(dir, "harness-capture.lock"), "sh", "-c", "echo locked; cat >/dev/null") //nolint:gosec // Fixed test command and an isolated temporary lock path.
	input, err := hold.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := hold.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "locked\n" {
		t.Fatalf("lock holder: %q %v", ready, err)
	}
	// The launcher's preparation, as the launcher runs it.
	box := harnesstest.Stage(t, ".").Install(t, nil)
	launchStub := "#!/bin/sh\nexit 0\n"
	box.Stub(t, "claude", launchStub)
	cmd := box.Command(t, "/usr/local/bin/discobox-harness-run")
	cmd.Env = append(cmd.Env, "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("initializer did not wait for lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	raw, err := os.ReadFile(state)
	if err != nil || string(raw) != initial {
		t.Fatal("state changed while handoff lock held")
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hold.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(state)
	if err != nil || !strings.Contains(string(raw), `"hasCompletedOnboarding": true`) {
		t.Fatal("initialization did not run after handoff released the lock")
	}
}
