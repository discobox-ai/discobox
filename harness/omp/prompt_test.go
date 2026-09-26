package omp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// `omp --print` says only what the model said, but a model asked for JSON may
// still fence it, so a schema'd answer goes through the image's answer helper
// (harness/DESIGN.md).

func TestOmpPromptNormalizesTheAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	verdict := `{"allow":false,"reason":"deleting a repository is not opening a PR"}`
	for _, tc := range []struct {
		name, printed, want string
	}{
		{"the answer as asked for", verdict, verdict},
		{"an answer the model fenced", "```json\n" + verdict + "\n```", verdict},
		{"the last thing said, after working through it", "Let me look at what was approved.\n" + verdict, verdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPrompt(t, tc.printed, 0, `{"type":"object"}`)
			if err != nil {
				t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
			}
			if out != tc.want {
				t.Fatalf("stdout = %q, want %q", out, tc.want)
			}
		})
	}
}

// An omp that fails is a wrapper that failed, whatever it printed first.
func TestOmpPromptFailsWhenOmpDoes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	for _, tc := range []struct {
		name    string
		printed string
		status  int
	}{
		{"an error and a non-zero exit", "Error: no provider", 4},
		{"nothing at all", "", 0},
		{"prose with no document in it", "I cannot decide that.", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runPrompt(t, tc.printed, tc.status, `{"type":"object"}`)
			if err == nil {
				t.Fatalf("stdout = %q, want the failure to reach the caller", out)
			}
			if out != "" {
				t.Fatalf("stdout = %q, want nothing printed", out)
			}
		})
	}
}

// The judge always asks with --no-tools, but the wrapper answers both ways, and
// the two branches are separate code: a failure has to reach the caller from
// either.
func TestOmpPromptFailsWhateverTheToolsRestriction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	verdict := `{"allow":true,"reason":"fine"}`
	for _, noTools := range []bool{true, false} {
		out, err := runPromptWith(t, "Error: no provider", 4, `{"type":"object"}`, noTools)
		if err == nil || out != "" {
			t.Fatalf("--no-tools=%v: stdout = %q, err = %v; want the failure to reach the caller", noTools, out, err)
		}
		out, err = runPromptWith(t, verdict, 0, `{"type":"object"}`, noTools)
		if err != nil || out != verdict {
			t.Fatalf("--no-tools=%v: stdout = %q, err = %v; want the answer", noTools, out, err)
		}
	}
}

// Without a schema nothing was promised about the shape of the answer, so the
// run is the ordinary one and its output is passed through.
func TestOmpPromptWithoutASchemaIsUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	out, err := runPrompt(t, "The change looks fine.\n\nNothing else to say.", 0, "")
	if err != nil {
		t.Fatalf("discobox-prompt error = %v", err)
	}
	if out != "The change looks fine.\n\nNothing else to say." {
		t.Fatalf("stdout = %q, want what omp printed", out)
	}
}

// runPrompt drives the wrapper against an omp that prints stdout and exits with
// status.
func runPrompt(t *testing.T, stdout string, status int, schema string) (string, error) {
	t.Helper()
	return runPromptWith(t, stdout, status, schema, true)
}

func runPromptWith(t *testing.T, stdout string, status int, schema string, noTools bool) (string, error) {
	t.Helper()
	dir := stubDir(t, "#!/bin/sh\ncat \"$DIR/stdout\"\nexit $STATUS\n")
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte(stdout+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"prompt.sh", "--model", "judge", "--system", "decide", "--prompt", "judge this"}
	if noTools {
		args = append(args, "--no-tools")
	}
	if schema != "" {
		args = append(args, "--output-schema", schema)
	}
	cmd := exec.CommandContext(t.Context(), "sh", args...)
	// HOME is this test's, so the wrapper reads no settings of the person
	// running it, and finds no model to name.
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+dir, "DIR="+dir, "STATUS="+strconv.Itoa(status))
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

// stubDir is a PATH holding the CLI this image installs, standing in for it,
// and the sandbox-agent image's answer helper the wrapper reaches for by name.
func stubDir(t *testing.T, omp string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "omp"), []byte(omp), 0o700); err != nil {
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
