package cli

import (
	"fmt"
	"os/exec"
	"testing"

	"github.com/obot-platform/discobox/execstream/client"
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
