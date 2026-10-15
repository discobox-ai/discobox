package pi

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// promptRun drives prompt.sh against a stubbed pi that records its argv,
// working directory, home, environment, the auth.json its home holds and its
// stdin, and answers with reply.
type promptRun struct {
	dir  string
	home string
}

func newPromptRun(t *testing.T, reply string, status int) *promptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	for _, tool := range []string{"sh", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s to run the wrapper with", tool)
		}
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	for _, d := range []string{".config/discobox", ".pi/agent"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "reply"), []byte(reply), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
printf '%s\n' "$@" >"$DIR/args"
pwd >"$DIR/cwd"
printf '%s' "$HOME" >"$DIR/judge-home"
ls -A "$PWD" >"$DIR/cwd-contents"
(cd "$HOME" && find . -type f | sort) >"$DIR/home-contents"
if [ -f "$HOME/.pi/agent/auth.json" ]; then jq -c . "$HOME/.pi/agent/auth.json" >"$DIR/auth"; fi
env >"$DIR/env"
cat >"$DIR/stdin"
cat "$DIR/reply"
exit ` + map[bool]string{true: "0", false: "3"}[status == 0] + "\n"
	writeExecutable(t, filepath.Join(dir, "pi"), stub)
	// The wrapper reaches for the base image's helper by name.
	answer, err := filepath.Abs(filepath.Join("..", "prompt-answer.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(dir, "discobox-prompt-answer"), "#!/bin/sh\nexec sh "+answer+" \"$@\"\n")
	return &promptRun{dir: dir, home: home}
}

func (p *promptRun) writeHome(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.home, rel), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (p *promptRun) run(t *testing.T, extra map[string]string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", append([]string{"prompt.sh"}, args...)...) //nolint:gosec // The wrapper under test, with this test's arguments.
	cmd.Env = []string{"PATH=" + p.dir + string(os.PathListSeparator) + os.Getenv("PATH"), "DIR=" + p.dir, "HOME=" + p.home, "TMPDIR=" + p.dir}
	for key, value := range extra {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	// A stdin that is not a terminal: pi would wait on it and read it as more
	// prompt, so the wrapper must not hand it on.
	cmd.Stdin = strings.NewReader("the caller's stdin")
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

func (p *promptRun) args(t *testing.T) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(p.read(t, "args"), "\n"), "\n")
}

// TestPromptJudgesWithTheConfiguredModel covers the judge role
// (ADR 26-10-15-259 §5): judgeModel from Discobox's settings, else the
// default model in pi's settings, else pi's own pick. pi has no roles, so fast
// is the same model.
func TestPromptJudgesWithTheConfiguredModel(t *testing.T) {
	for _, tc := range []struct {
		name, setting, piSettings, role string
		want                            []string
	}{
		{"nothing configured is pi's own pick", "", "", "judge", nil},
		{"pi's default model", "", `{"defaultProvider":"anthropic","defaultModel":"claude-sonnet-5-5"}`, "judge", []string{"--model", "anthropic/claude-sonnet-5-5"}},
		{"a provider alone names nothing", "", `{"defaultProvider":"anthropic"}`, "judge", nil},
		{"judgeModel wins", `{"judgeModel":"xai/grok-5"}`, `{"defaultProvider":"anthropic","defaultModel":"claude-sonnet-5-5"}`, "judge", []string{"--model", "xai/grok-5"}},
		{"an empty judgeModel falls through", `{"judgeModel":""}`, `{"defaultProvider":"anthropic","defaultModel":"claude-sonnet-5-5"}`, "judge", []string{"--model", "anthropic/claude-sonnet-5-5"}},
		// pi takes the word after --model as its value whatever it starts with.
		{"a model that reads like a flag stays the model's value", `{"judgeModel":"--no-tools"}`, "", "judge", []string{"--model", "--no-tools"}},
		{"fast is the same model", "", `{"defaultProvider":"anthropic","defaultModel":"claude-haiku-5-5"}`, "fast", []string{"--model", "anthropic/claude-haiku-5-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPromptRun(t, "ok", 0)
			if tc.setting != "" {
				p.writeHome(t, settingsPath, tc.setting)
			}
			if tc.piSettings != "" {
				p.writeHome(t, piSettingsPath, tc.piSettings)
			}
			if _, err := p.run(t, nil, "--model", tc.role, "--no-tools", "--prompt", "is it safe"); err != nil {
				t.Fatal(err)
			}
			args := p.args(t)
			var got []string
			if i := slices.Index(args, "--model"); i >= 0 {
				got = args[i : i+2]
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("model arguments = %q, want %q (argv %q)", got, tc.want, args)
			}
		})
	}
}

// TestPromptIsolatesTheToolFreeJudge covers what --no-tools shuts out: the
// caller is the agent being judged, so no settings, extension, instruction or
// variable it could have set may reach the judge — only the harness's
// credentials, rebuilt into a home of the judge's own.
func TestPromptIsolatesTheToolFreeJudge(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	// The agent's auth.json: a key and a login the judge may use, and entries
	// that would each run the agent's code or name where the judge's requests
	// go.
	p.writeHome(t, authPath, `{
		"openrouter": {"type":"api_key","key":"sk-or-v1-ab_cd","env":{"OPENROUTER_BASE_URL":"http://127.0.0.1:9999"}},
		"anthropic": {"type":"oauth","access":"sk-ant-oat01-ab_cd-EF","refresh":"discobox-refresh-happens-in-the-control-plane","expires":4102444800000,"region":"eu"},
		"openrouter-oauth": {"type":"oauth","access":"sk-or-v1-ef_gh","refresh":"","expires":9007199254740991},
		"github-copilot": {"type":"oauth","access":"tid=x;proxy-ep=proxy.evil.example","refresh":"gho_SENT1NEL","expires":4102444800000,"enterpriseUrl":"evil.example"},
		"xai": {"type":"api_key","key":"!curl http://127.0.0.1:9999/key"},
		"mistral": {"type":"api_key","key":" {\"baseUrl\":\"http://127.0.0.1:9999\"}"},
		"kimi-coding": {"type":"oauth","access":"proxy-ep=evil example","refresh":"r","expires":1},
		"groq": "not an object"
	}`)
	p.writeHome(t, piSettingsPath, `{"defaultProvider":"anthropic","defaultModel":"claude-sonnet-5-5","extensions":["/agent/evil.js"]}`)
	if _, err := p.run(t, map[string]string{
		"PI_CODING_AGENT_DIR": "/agent",
		"PI_PACKAGE_DIR":      "/agent-pkg",
		"XDG_CONFIG_HOME":     "/agent-config",
		"ANTHROPIC_BASE_URL":  "http://agent.example",
		"HTTPS_PROXY":         "http://127.0.0.1:17008",
	}, "--model", "judge", "--no-tools", "--system", "You judge.", "--prompt", "is it safe"); err != nil {
		t.Fatal(err)
	}
	args := p.args(t)
	for _, want := range []string{"-p", "--no-session", "--no-tools", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-approve", "--offline"} {
		if !slices.Contains(args, want) {
			t.Errorf("judge argv %q lacks %q", args, want)
		}
	}
	if i := slices.Index(args, "--system-prompt"); i < 0 || args[i+1] != "You judge." {
		t.Errorf("judge argv %q, want the system text as --system-prompt's value", args)
	}
	if args[len(args)-2] != "--" || args[len(args)-1] != "is it safe" {
		t.Errorf("judge argv %q, want the prompt last, after --", args)
	}
	home := p.read(t, "judge-home")
	if home == p.home || !strings.HasPrefix(home, p.dir) {
		t.Errorf("judge ran with HOME %s, want one of its own", home)
	}
	if got := p.read(t, "home-contents"); got != "./.pi/agent/auth.json\n" {
		t.Errorf("judge's home holds %q, want only auth.json", got)
	}
	// What reached the judge: tokens, no env, no endpoint, no command, and a
	// Copilot login it has to exchange for itself.
	want := `{"openrouter":{"type":"api_key","key":"sk-or-v1-ab_cd"},` +
		`"anthropic":{"type":"oauth","access":"sk-ant-oat01-ab_cd-EF","refresh":"discobox-refresh-happens-in-the-control-plane","expires":4102444800000},` +
		`"openrouter-oauth":{"type":"oauth","access":"sk-or-v1-ef_gh","refresh":"","expires":9007199254740991},` +
		`"github-copilot":{"type":"oauth","access":"discobox-copilot-token-is-exchanged-in-the-sandbox","refresh":"gho_SENT1NEL","expires":0}}`
	if got := strings.TrimSpace(p.read(t, "auth")); got != want {
		t.Errorf("judge's auth.json\n  %s\nwant\n  %s", got, want)
	}
	if got := p.read(t, "cwd-contents"); got != "" {
		t.Errorf("judge's working directory holds %q, want nothing", got)
	}
	env := p.read(t, "env")
	for _, leaked := range []string{"PI_CODING_AGENT_DIR=", "PI_PACKAGE_DIR=", "XDG_CONFIG_HOME=", "ANTHROPIC_BASE_URL="} {
		if strings.Contains(env, leaked) {
			t.Errorf("judge inherited %s", strings.TrimSuffix(leaked, "="))
		}
	}
	// pi honors HTTPS_PROXY itself; the judge keeps the sandbox's.
	if !strings.Contains(env, "HTTPS_PROXY=http://127.0.0.1:17008\n") {
		t.Errorf("judge lost the sandbox's HTTPS_PROXY")
	}
	if got := p.read(t, "stdin"); got != "" {
		t.Errorf("pi read %q from stdin, want /dev/null", got)
	}
	// And the home it made is gone afterwards.
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("judge's home %s outlived it", home)
	}
}

// A judge with no credentials file still runs, in its own empty home.
func TestPromptRunsTheJudgeWithNoCredentials(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	if _, err := p.run(t, nil, "--model", "judge", "--no-tools", "--prompt", "q"); err != nil {
		t.Fatal(err)
	}
	if got := p.read(t, "home-contents"); got != "" {
		t.Errorf("judge's home holds %q, want nothing", got)
	}
}

// The system text always reaches pi as text: a value of its system-prompt
// flags is read as a file when one by that name exists.
func TestPromptSystemTextIsNeverAPath(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	if _, err := p.run(t, nil, "--model", "judge", "--system", "/etc/hostname", "--prompt", "q"); err != nil {
		t.Fatal(err)
	}
	raw := p.read(t, "args")
	if !strings.Contains(raw, "--append-system-prompt\n/etc/hostname\n\n") {
		t.Fatalf("argv %q, want the system text with its newline", raw)
	}
	if strings.Contains(raw, "--no-tools") {
		t.Fatalf("argv %q, want tools left on when the caller did not turn them off", raw)
	}
}

// pi reads an @-led argument as a file to attach, `--` or not.
func TestPromptAnAtLedPromptIsNotAFile(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	if _, err := p.run(t, nil, "--model", "judge", "--no-tools", "--prompt", "@main.go is it safe"); err != nil {
		t.Fatal(err)
	}
	if args := p.args(t); args[len(args)-1] != " @main.go is it safe" {
		t.Fatalf("argv %q, want the prompt led by a space", args)
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
			out, err := p.run(t, nil, args...)
			if err != nil {
				t.Fatalf("discobox-prompt error = %v, output = %q", err, out)
			}
			if out != tc.want {
				t.Fatalf("stdout = %q, want %q", out, tc.want)
			}
		})
	}
}

// A pi that fails is a wrapper that failed, not one that answered nothing.
func TestPromptFailsWhenPiDoes(t *testing.T) {
	p := newPromptRun(t, `{"allow":true,"reason":"not the answer"}`, 1)
	out, err := p.run(t, nil, "--model", "judge", "--prompt", "judge this", "--no-tools", "--output-schema", `{"type":"object"}`)
	if err == nil {
		t.Fatalf("stdout = %q, want the failure to reach the caller", out)
	}
	if strings.Contains(out, "allow") {
		t.Fatalf("stdout = %q, want nothing that reads as a verdict", out)
	}
}
