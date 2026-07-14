package cli

import "testing"

func TestAttachExitFrameReturnsExitCode(t *testing.T) {
	err := attachExitErrorFromPayload("sandbox exec", []byte(`{"status":"failed","exitCode":7}`))
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
	if err := attachExitErrorFromPayload("harness terminal", []byte(`{"status":"exited","exitCode":0}`)); err != nil {
		t.Fatalf("exit frame error = %v, want nil", err)
	}
}
