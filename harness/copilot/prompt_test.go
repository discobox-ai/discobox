package copilot

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// promptRun drives prompt.sh against a stubbed copilot that records its argv,
// working directory and environment, and answers with reply.
type promptRun struct {
	dir string
}

func newPromptRun(t *testing.T, reply string, status int) *promptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reply"), []byte(reply), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
printf '%s\n' "$@" >"$DIR/args"
pwd >"$DIR/cwd"
printf '%s' "${COPILOT_HOME:-}" >"$DIR/home"
printf '%s' "${COPILOT_ALLOW_ALL:-}" >"$DIR/allow-all"
ls -A "$PWD" >"$DIR/cwd-contents"
cat "$DIR/reply"
exit ` + map[bool]string{true: "0", false: "3"}[status == 0] + "\n"
	writeExecutable(t, filepath.Join(dir, "copilot"), stub)
	// The wrapper reaches for the base image's helper by name.
	answer, err := filepath.Abs(filepath.Join("..", "prompt-answer.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(dir, "discobox-prompt-answer"), "#!/bin/sh\nexec sh "+answer+" \"$@\"\n")
	return &promptRun{dir: dir}
}

func (p *promptRun) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{"prompt.sh"}, args...)...) //nolint:gosec // The wrapper under test, with this test's arguments.
	cmd.Env = append(os.Environ(),
		"PATH="+p.dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DIR="+p.dir,
		"COPILOT_ALLOW_ALL=true",
	)
	out, err := cmd.Output()
	return strings.TrimRight(string(out), "\n"), err
}

func (p *promptRun) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A judge has nothing to call and nothing the judged agent wrote to read: no
// tools (an allowlist naming none, since an empty one offers every tool), no
// custom instructions or built-in MCP servers, a home and working directory of
// its own, and no COPILOT_ALLOW_ALL to trust that directory.
func TestPromptJudgesWithNoToolsInAHomeOfItsOwn(t *testing.T) {
	p := newPromptRun(t, `{"allow":true,"reason":"ok"}`, 0)
	if _, err := p.run(t, "--model", "judge", "--system", "decide", "--prompt", "judge this", "--no-tools"); err != nil {
		t.Fatal(err)
	}
	args := p.read(t, "args")
	for _, want := range []string{
		"--available-tools=discobox-no-tools\n",
		"--no-custom-instructions\n",
		"--disable-builtin-mcps\n",
		"--silent\n",
		"--model\nclaude-sonnet-5.5\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("copilot args missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--available-tools=\n") || strings.Contains(args, "--allow-all") {
		t.Errorf("copilot args offer tools:\n%s", args)
	}
	// The system text leads the prompt and the schema-free ask ends it.
	if !strings.Contains(args, "--prompt=decide\n\njudge this") {
		t.Errorf("copilot was not handed the system text ahead of the prompt:\n%s", args)
	}
	home := p.read(t, "home")
	if home == "" {
		t.Fatal("copilot ran in the shared COPILOT_HOME, want one of the run's own")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("the run's home %s is still there after copilot exited", home)
	}
	if got := strings.TrimSpace(p.read(t, "cwd-contents")); got != "" {
		t.Errorf("copilot ran in a directory holding %q, want an empty one", got)
	}
	if got := p.read(t, "allow-all"); got != "" {
		t.Errorf("COPILOT_ALLOW_ALL = %q in the judge, want it unset", got)
	}
}

// The verdict a schema'd ask returns is one JSON document and nothing else
// (harness/DESIGN.md), even when the model fences it.
func TestPromptPrintsTheAnswerAlone(t *testing.T) {
	verdict := `{"allow":true,"reason":"opening the PR it was approved for"}`
	for _, tc := range []struct{ name, reply, schema, want string }{
		{"a schema'd answer", verdict, `{"type":"object"}`, verdict},
		{"a schema'd answer in a fence", "```json\n" + verdict + "\n```", `{"type":"object"}`, verdict},
		{"no schema, so the reply as it is", "The change looks fine to me.", "", "The change looks fine to me."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPromptRun(t, tc.reply, 0)
			args := []string{"--model", "judge", "--prompt", "judge this", "--no-tools"}
			if tc.schema != "" {
				args = append(args, "--output-schema", tc.schema)
			}
			out, err := p.run(t, args...)
			if err != nil {
				t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
			}
			if out != tc.want {
				t.Fatalf("stdout = %q, want %q", out, tc.want)
			}
		})
	}
}

// A copilot that fails is a wrapper that failed, not one that answered nothing.
func TestPromptFailsWhenCopilotDoes(t *testing.T) {
	p := newPromptRun(t, `{"allow":true,"reason":"not the answer"}`, 1)
	out, err := p.run(t, "--model", "judge", "--prompt", "judge this", "--no-tools", "--output-schema", `{"type":"object"}`)
	if err == nil {
		t.Fatalf("stdout = %q, want the failure to reach the caller", out)
	}
	if strings.Contains(out, "allow") {
		t.Fatalf("stdout = %q, want nothing that reads as a verdict", out)
	}
}
