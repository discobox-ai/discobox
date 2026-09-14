package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCodexPromptUsesGlobalApprovalAndReturnsOnlyFinalJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX image wrapper")
	}
	dir := t.TempDir()
	stub := `#!/bin/sh
set -eu
[ "$1" = --ask-for-approval ] && [ "$2" = never ] && [ "$3" = exec ] || exit 9
shift 3
answer=""
schema=""
while [ $# -gt 0 ]; do
 case "$1" in
 --output-last-message) answer="$2"; shift 2 ;;
 --output-schema) schema="$2"; shift 2 ;;
 --ask-for-approval) exit 10 ;;
 *) shift ;;
 esac
done
[ -s "$schema" ]
printf 'CLI transcript must not reach the judge decoder\n'
printf '{"allow":true,"reason":"associated"}\n' > "$answer"
`
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	command := exec.CommandContext(t.Context(), "sh", "codex-cli/prompt.sh", "--model", "judge", "--prompt", "read repository status", "--output-schema", `{"type":"object"}`, "--no-tools")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v: %s", err, output)
	}
	if string(output) != "{\"allow\":true,\"reason\":\"associated\"}\n" {
		t.Fatalf("not a standalone verdict: %q", output)
	}
}
