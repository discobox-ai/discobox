package sandboxruntime

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
)

// lockChildEnv names, in a child of this test binary, the root whose
// sandbox sbx_1 it holds the power lock and a boot of (lockChild).
const lockChildEnv = "DISCOBOX_TEST_POWER_LOCK_CHILD"

// lockChild is the other agent: a process of its own, holding sbx_1's power
// lock and a boot of it on the root named by lockChildEnv, until its stdin
// closes. It says "held" once both are.
func lockChild() {
	root := layout.ContainerAt(os.Getenv(lockChildEnv))
	core := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	unlock, err := core.lockPower(context.Background(), "sbx_1")
	if err != nil {
		os.Exit(2)
	}
	boot := core.beginBoot("sbx_1")
	_, _ = os.Stdout.WriteString("held\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
	core.endBoot("sbx_1", boot, nil)
	unlock()
	os.Exit(0)
}

// Two agents of a pool in processes of their own, on one run directory: the
// other's power lock keeps this one's waiting, and its boot reads as under
// way here, until that process lets go.
func TestThePowerLockAndBootMarkHoldAcrossProcesses(t *testing.T) {
	if os.Getenv(lockChildEnv) != "" {
		lockChild()
	}
	dir := t.TempDir()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestThePowerLockAndBootMarkHoldAcrossProcesses$") //nolint:gosec // G204: this test binary, re-run as the other agent
	child.Env = append(os.Environ(), lockChildEnv+"="+dir)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("the other agent did not take the lock: %q, %v", line, err)
	}

	core := &sandboxCore{root: layout.ContainerAt(dir), projectID: "prj", poolID: "pool"}
	if _, locked, err := core.tryLockPower("sbx_1"); err != nil || locked {
		t.Fatalf("tryLockPower with the other agent holding it = %v, %v; want not locked", locked, err)
	}
	if !core.SandboxBooting("sbx_1") {
		t.Fatal("the other agent's boot does not read as under way here")
	}
	waited := make(chan error, 1)
	go func() {
		unlock, err := core.lockPower(context.Background(), "sbx_1")
		if err == nil {
			unlock()
		}
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("lockPower returned (%v) while the other agent holds the lock", err)
	case <-time.After(300 * time.Millisecond):
	}

	_ = stdin.Close()
	if err := child.Wait(); err != nil {
		t.Fatalf("the other agent: %v", err)
	}
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("lockPower once the other agent let go: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lockPower still waits after the other agent exited")
	}
	if core.SandboxBooting("sbx_1") {
		t.Fatal("the sandbox reads booting after the other agent's boot ended")
	}
}
