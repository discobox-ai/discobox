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

// The judge answers without extended thinking and without a session's
// background traffic, whatever the environment it was started in said: both
// are seconds of a held request (harness/claude-code/prompt.sh).
func TestClaudePromptJudgesWithoutThinking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	dir := stubDir(t, "#!/bin/sh\necho \"thinking=$MAX_THINKING_TOKENS traffic=$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\"\n")
	t.Setenv("MAX_THINKING_TOKENS", "31999")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
	out, err := runPrompt(t, dir, "")
	if err != nil {
		t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
	}
	if want := "thinking=0 traffic=1"; out != want {
		t.Fatalf("claude ran with %q, want %q", out, want)
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

// A judge answers asks in parallel, one claude per ask, so a run that judges
// keeps what claude writes in a configuration home of its own. It starts with
// the account the harness installed, and it goes when claude exits, leaving
// the shared home as it was.
func TestClaudePromptJudgesInAHomeOfItsOwn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // Stand-ins for the files the harness installs, holding no credential.
	for name, content := range map[string]string{".claude.json": `{"primaryApiKey":"sentinel"}`, ".claude/.credentials.json": `{"claudeAiOauth":{}}`} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The stub says where it ran and what it found, and writes what claude
	// would: its own rewrite of .claude.json.
	dir := stubDir(t, "#!/bin/sh\nprintf '%s\\n' \"$CLAUDE_CONFIG_DIR\" >\"$DIR/home\"\n"+
		"ls -A \"$CLAUDE_CONFIG_DIR\" >\"$DIR/found\"\necho '{\"rewritten\":true}' >\"$CLAUDE_CONFIG_DIR/.claude.json\"\n"+
		"echo '{\"allow\":true,\"reason\":\"fine\"}'\n")
	t.Setenv("HOME", home)
	if out, err := runPrompt(t, dir, `{"type":"object"}`); err != nil {
		t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
	}
	ran, _ := os.ReadFile(filepath.Join(dir, "home"))
	own := strings.TrimSpace(string(ran))
	if own == "" || strings.HasPrefix(own, home) {
		t.Fatalf("claude ran with CLAUDE_CONFIG_DIR %q, want a home of the run's own", own)
	}
	found, _ := os.ReadFile(filepath.Join(dir, "found"))
	if got := strings.Fields(string(found)); strings.Join(got, " ") != ".claude.json .credentials.json" {
		t.Fatalf("the run's home held %q, want the account and nothing else", got)
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("the run's home %s is still there after claude exited", own)
	}
	if kept, _ := os.ReadFile(filepath.Join(home, ".claude.json")); string(kept) != `{"primaryApiKey":"sentinel"}` {
		t.Fatalf("the shared .claude.json is now %q, want it as installed", kept)
	}
}
