package cli

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/execstream/client"
)

func TestAttachExitFrameReturnsExitCode(t *testing.T) {
	err := client.ExitErrorFromPayload("sandbox exec", []byte(`{"status":"failed","exitCode":7}`))
	if err == nil {
		t.Fatal("exit frame error = nil")
	}
	code, ok := ExitCode(err)
	if !ok {
		t.Fatalf("ExitCode(%v) ok = false, want true", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
}

func TestAttachExitFrameZeroExitIsSuccess(t *testing.T) {
	if err := client.ExitErrorFromPayload("harness terminal", []byte(`{"status":"exited","exitCode":0}`)); err != nil {
		t.Fatalf("exit frame error = %v, want nil", err)
	}
}

// A failing helper subprocess, such as git rev-parse outside a repository,
// reports an *exec.ExitError. Exiting silently with its status would hide the
// error message, so only an attached process's exit status drives the CLI exit
// code.
func TestExitCodeIgnoresSubprocessExitErrors(t *testing.T) {
	subprocessErr := exec.CommandContext(t.Context(), "false").Run()
	if subprocessErr == nil {
		t.Fatal("running false: got nil error, want failure")
	}
	if code, ok := ExitCode(fmt.Errorf("resolve git root: %w", subprocessErr)); ok {
		t.Fatalf("ExitCode(subprocess error) = %d, true; want ok = false so the message is printed", code)
	}
}

// The local escape from a stalled attach exits the way a shell reports an
// interrupted command, and silently: the notice already said what happened.
func TestInterruptedExitIsASilent130(t *testing.T) {
	code, ok := ExitCode(interruptedExit())
	if !ok || code != 130 {
		t.Fatalf("ExitCode(interruptedExit()) = %d, %t; want 130, true", code, ok)
	}
}

// The notice ends its lines the way the attach's terminal mode requires: a raw
// terminal has no automatic carriage return, so a bare \n would stair-step the
// message across the screen the caller is already staring at.
func TestInterruptNoticeEndsLinesForTheTerminalMode(t *testing.T) {
	for _, tc := range []struct {
		raw  bool
		want string
	}{
		{raw: true, want: "\r\nNot responding: your interrupt has not reached the discobox.\r\nPress Ctrl-C again to quit.\r\n"},
		{raw: false, want: "\nNot responding: your interrupt has not reached the discobox.\nPress Ctrl-C again to quit.\n"},
	} {
		var out strings.Builder
		interruptNotice(&out, tc.raw, "the discobox")()
		if out.String() != tc.want {
			t.Fatalf("notice(raw=%t) = %q, want %q", tc.raw, out.String(), tc.want)
		}
	}
}
