package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/localpty"
)

// A command nobody spawned is nobody's to end. Only the launcher says whose
// child a command is, and an invocation the user typed themselves outlives
// whatever shell started it for as long as they want it to.
func TestAnInvocationWithNoParentNamedIsNotWatched(t *testing.T) {
	for _, value := range []string{"", "not a pid", "0", "1", "-3"} {
		t.Setenv(localpty.ParentPIDEnv, value)
		ctx := context.Background()
		if got := watchParentProcess(ctx, nil); got != ctx {
			t.Fatalf("%q: the command should run under the context it was given, unwatched", value)
		}
	}
}

// The launcher's own children are watched, and the pid it named is the one
// watched.
func TestAPaneChildIsWatched(t *testing.T) {
	t.Setenv(localpty.ParentPIDEnv, strconv.Itoa(os.Getppid()))
	noExit(t)
	ctx := t.Context()
	watched := watchParentProcess(ctx, nil)
	if watched == ctx {
		t.Fatal("a command started on a pane's pty should be watched")
	}
	if watched.Err() != nil {
		t.Fatalf("a live parent should leave the command running: %v", watched.Err())
	}
}

// A parent that goes away ends the command. This is the whole point: the pane's
// two ways of ending a child both run inside the launcher, so a launcher that
// dies without unwinding leaves the child to run forever.
func TestALostParentEndsTheCommand(t *testing.T) {
	exited := make(chan int, 1)
	restore := parentWatchExit
	parentWatchExit = func(code int) { exited <- code }
	t.Cleanup(func() { parentWatchExit = restore })

	var notice bytes.Buffer
	ctx := watchProcess(t.Context(), exitedProcess(t), time.Millisecond, time.Millisecond, &notice)

	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("losing the parent should have canceled the command's context")
	}
	select {
	case code := <-exited:
		if code == 0 {
			t.Fatalf("exit = %d, want a failure: the command did not finish, it was abandoned", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a command that did not unwind should be made to leave anyway")
	}
	if notice.Len() == 0 {
		t.Fatal("a command that ends itself should say why")
	}
}

// Canceling for any other reason stops the watch rather than leaving a
// goroutine polling a pid for the life of the process.
func TestTheWatchEndsWithTheCommand(t *testing.T) {
	restore := parentWatchExit
	parentWatchExit = func(int) { t.Error("a command that ended on its own should not be exited by the watchdog") }
	t.Cleanup(func() { parentWatchExit = restore })

	// A poll that will not come round again, so the only branch left is the
	// one being tested: what the watch does when the command ends first.
	ctx, cancel := context.WithCancel(context.Background())
	watched := watchProcess(ctx, os.Getppid(), time.Hour, time.Millisecond, nil)
	cancel()
	select {
	case <-watched.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("canceling the command should end the watch")
	}
	// Long enough for a watchdog that kept polling to have fired.
	time.Sleep(50 * time.Millisecond)
}

// The watch is wired into every command, so what a command runs under is the
// watched context and not the one Execute was handed.
func TestTheWatchedContextIsWhatCommandsRunUnder(t *testing.T) {
	t.Setenv(localpty.ParentPIDEnv, strconv.Itoa(os.Getppid()))
	noExit(t)

	cmd, _ := newRootCommand()
	var got context.Context
	cmd.AddCommand(&cobra.Command{
		Use: "parentwatch-probe",
		RunE: func(cmd *cobra.Command, _ []string) error {
			got = cmd.Context()
			return nil
		},
	})
	cmd.SetArgs([]string{"parentwatch-probe"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	ctx := t.Context()
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("the command did not run")
	}
	if got == ctx {
		t.Fatal("the command ran under the unwatched context it was launched with")
	}
}

// The pid names one process's child and nothing further down. Everything this
// command starts inherits its environment, including the autolaunched server —
// a singleton that outlives every CLI on the machine, and which would find a
// pid that is not its parent already "gone".
func TestTheParentPIDDoesNotReachWhatTheCommandStarts(t *testing.T) {
	t.Setenv(localpty.ParentPIDEnv, strconv.Itoa(os.Getppid()))
	noExit(t)
	watchParentProcess(t.Context(), nil)
	if value, ok := os.LookupEnv(localpty.ParentPIDEnv); ok {
		t.Fatalf("%s = %q in the environment a child would inherit, want it consumed", localpty.ParentPIDEnv, value)
	}
}

// noExit keeps a watch started by a test from ending the test binary.
//
// A test that starts a real watch leaves a goroutine polling for as long as its
// context lives, and what that goroutine does when it decides the parent is
// gone is exit the process. Whether it decides that is a question about the
// machine the test is running on — what a runner reports for a parent it is
// allowed to open — and no test in this package is asking it.
func noExit(t *testing.T) {
	t.Helper()
	restore := parentWatchExit
	parentWatchExit = func(int) {}
	t.Cleanup(func() { parentWatchExit = restore })
}

// exitedProcess is a pid that named a process and no longer does.
//
// One is spawned and reaped rather than picked, because what "gone" means is
// exactly what differs between the platforms: Unix answers by asking who this
// process's parent is now, Windows by probing the pid itself. A pid that is
// merely not our parent — this process's own, say — is gone under the first and
// plainly alive under the second, which is a test that passes on one platform
// while asserting nothing on the other.
//
// The process is this test binary in its no-op helper mode (see TestMain),
// which is how this package spawns a real process portably: Windows cannot exec
// a shell script.
func exitedProcess(t *testing.T) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), exe)
	cmd.Env = append(os.Environ(), fakeEditorEnv+"=")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a process to outlive: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for it to end: %v", err)
	}
	return pid
}
