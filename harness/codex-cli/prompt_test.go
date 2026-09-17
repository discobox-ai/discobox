package codexcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptNoToolsUsesSupportedCodexApprovalConfig(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	stub := filepath.Join(dir, "codex")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CODEX_ARGS_FILE\"\nprintf '{\"allow\":true,\"reason\":\"ok\"}\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "sh", "prompt.sh", "--model", "judge", "--prompt", "test", "--no-tools")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "CODEX_ARGS_FILE="+argsFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prompt.sh failed: %v: %s", err, out)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(args)
	for _, want := range []string{"exec\n", "--model\ngpt-5.6-terra\n", "--sandbox\nread-only\n", "--config\napproval_policy=never\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("codex args missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--ask-for-approval") {
		t.Errorf("codex exec does not accept --ask-for-approval:\n%s", got)
	}
}
