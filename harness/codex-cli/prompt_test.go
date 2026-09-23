package codexcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// The verdict a schema'd ask returns is one JSON document and nothing else
// (harness/DESIGN.md), which for codex means the agent's last message rather
// than the narration it prints while working, and the document inside that
// message rather than the fence it may arrive in.
func TestCodexPromptPrintsTheAnswerAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	verdict := `{"allow":true,"reason":"opening the PR it was approved for"}`
	for _, tc := range []struct {
		name, message, schema, want string
	}{
		{"a schema'd answer", verdict, `{"type":"object"}`, verdict},
		{"a schema'd answer in a fence", "```json\n" + verdict + "\n```", `{"type":"object"}`, verdict},
		{"no schema, so the message as it is", "The change looks fine to me.", "", "The change looks fine to me."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPrompt(t, tc.message, tc.schema)
			if err != nil {
				t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
			}
			if out != tc.want {
				t.Fatalf("stdout = %q, want %q", out, tc.want)
			}
		})
	}
}

// A codex that fails is a wrapper that failed, not one that answered nothing:
// its narration must not become the answer.
func TestCodexPromptFailsWhenCodexDoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	dir := t.TempDir()
	writeStub(t, dir, "codex", `#!/bin/sh
echo '| thinking {"allow":true,"reason":"not the answer"}'
exit 3
`)
	out, err := runPromptIn(t, dir, `{"type":"object"}`)
	if err == nil {
		t.Fatalf("stdout = %q, want the failure to reach the caller", out)
	}
	if strings.Contains(out, "allow") {
		t.Fatalf("stdout = %q, want nothing that reads as a verdict", out)
	}
}

// runPrompt drives the wrapper against a codex that writes message to the file
// named by --output-last-message and narrates on stdout, as the real one does.
func runPrompt(t *testing.T, message, schema string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "message"), []byte(message+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStub(t, dir, "codex", `#!/bin/sh
# The real codex narrates its run on stdout and writes its last message to the
# file it is given.
out=""
while [ $# -gt 0 ]; do
	case "$1" in
	--output-last-message) out="$2"; shift 2 ;;
	*) shift ;;
	esac
done
echo "| Reading the request…"
echo "| Deciding."
[ -n "$out" ] && cat "$DIR/message" >"$out"
exit 0
`)
	return runPromptIn(t, dir, schema)
}

func runPromptIn(t *testing.T, dir, schema string) (string, error) {
	t.Helper()
	args := []string{"prompt.sh", "--model", "judge", "--system", "decide", "--prompt", "judge this", "--no-tools"}
	if schema != "" {
		args = append(args, "--output-schema", schema)
	}
	cmd := exec.CommandContext(t.Context(), "sh", args...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "DIR="+dir)
	stdout, err := cmd.Output()
	return strings.TrimRight(string(stdout), "\n"), err
}

// writeStub puts an executable on the wrapper's PATH, standing in for the CLI
// the image installs.
func writeStub(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// The wrapper reaches for the base image's helper by name.
	answer, err := filepath.Abs(filepath.Join("..", "prompt-answer.sh"))
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexec sh " + answer + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "discobox-prompt-answer"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
}
