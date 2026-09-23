package claudecode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A schema'd ask returns one JSON document and nothing else
// (harness/DESIGN.md). `claude --print` adds no transcript, but a model asked
// for JSON may still fence it, and the promise is the wrapper's to keep.
func TestClaudePromptPrintsTheAnswerAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	verdict := `{"allow":true,"reason":"reading the branches supports opening the PR"}`
	for _, tc := range []struct {
		name, said, schema, want string
	}{
		{"an answer as asked for", verdict, `{"type":"object"}`, verdict},
		{"an answer the model fenced anyway", "```json\n" + verdict + "\n```", `{"type":"object"}`, verdict},
		{"no schema, so what the model said", "Looks fine to me.", "", "Looks fine to me."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := stubDir(t, "#!/bin/sh\ncat \"$DIR/said\"\n")
			if err := os.WriteFile(filepath.Join(dir, "said"), []byte(tc.said+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runPrompt(t, dir, tc.schema)
			if err != nil {
				t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
			}
			if out != tc.want {
				t.Fatalf("stdout = %q, want %q", out, tc.want)
			}
		})
	}
}

// A claude that fails is a wrapper that failed, whatever it printed first.
func TestClaudePromptFailsWhenClaudeDoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	dir := stubDir(t, "#!/bin/sh\necho '{\"allow\":true,\"reason\":\"not the answer\"}'\nexit 5\n")
	out, err := runPrompt(t, dir, `{"type":"object"}`)
	if err == nil {
		t.Fatalf("stdout = %q, want the failure to reach the caller", out)
	}
	if strings.Contains(out, "allow") {
		t.Fatalf("stdout = %q, want nothing that reads as a verdict", out)
	}
}

func runPrompt(t *testing.T, dir, schema string) (string, error) {
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

// stubDir is a PATH holding the CLI this image installs, standing in for it,
// and the sandbox-agent image's answer helper the wrapper reaches for by name.
func stubDir(t *testing.T, claude string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(claude), 0o700); err != nil {
		t.Fatal(err)
	}
	answer, err := filepath.Abs(filepath.Join("..", "prompt-answer.sh"))
	if err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\nexec sh " + answer + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "discobox-prompt-answer"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
