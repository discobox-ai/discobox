//go:build darwin

package execs

// Temporary, for #1139's proof on the darwin runner, reverted before merge:
// StopATerminal as it was, stopping the shell as soon as a file written from
// its rc file appears, so the darwin job can count how often that misses the
// hangup next to the prompt-based subtest.

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStopATerminalAfterItsRCFile(t *testing.T) {
	bash, err := osexec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	agent := startContractAgent(t, func(_ *testing.T, runtimeDir string) UnitManager {
		return NewSupervisor(filepath.Join(runtimeDir, supervisorDirName))
	}, contractRuntimeDir(t), newContractAudit())
	dir := t.TempDir()
	ready := filepath.Join(dir, "interactive")
	rc := filepath.Join(dir, "rc")
	if err := os.WriteFile(rc, fmt.Appendf(nil, ": > %q\n", ready), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exec, err := agent.manager.Create(ctx, CreateRequest{
		Command: []string{bash, "--noprofile", "--rcfile", rc, "-i"},
		TTY:     true,
		Rows:    24,
		Cols:    80,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := agent.manager.Start(ctx, exec.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	running := agent.waitFor(t, exec.ID, "running", func(exec Exec) bool {
		return exec.Status == StatusRunning && exec.PID > 0
	})
	waitUntil(t, "the shell to be interactive", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	began := time.Now()
	if _, err := agent.manager.Stop(context.Background(), exec.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(began); took >= shimStopGrace/2 {
		t.Fatalf("stopping a terminal took %s; %s", took, shimLog(agent.units, exec.Unit))
	}
	waitUntil(t, "the terminal's session to exit", func() bool {
		return sessionGone(int(running.PID))
	})
}
