package omp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// promptRun drives prompt.sh against a stubbed omp. `omp config get
// modelRoles --json` answers $ROLES; `omp auth import FILE` copies FILE into
// the home it runs with, so what the judge's home holds can be read back; any
// other call records its argv, working directory, home and environment, reads
// its stdin, and answers with reply.
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
	if err := os.MkdirAll(filepath.Join(home, ".config", "discobox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reply"), []byte(reply), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
case "$*" in
"config get modelRoles --json")
	pwd >"$DIR/config-cwd"
	printf '{"key":"modelRoles","value":%s}\n' "${ROLES:-{\}}"
	exit 0 ;;
"auth import "*)
	cp "$3" "$HOME/imported.json"
	jq -c . "$3" >"$DIR/imported"
	exit 0 ;;
esac
printf '%s\n' "$@" >"$DIR/args"
pwd >"$DIR/cwd"
printf '%s' "$HOME" >"$DIR/judge-home"
ls -A "$PWD" >"$DIR/cwd-contents"
ls -A "$HOME" >"$DIR/home-contents"
env >"$DIR/env"
cat >"$DIR/stdin"
cat "$DIR/reply"
exit ` + map[bool]string{true: "0", false: "3"}[status == 0] + "\n"
	writeExecutable(t, filepath.Join(dir, "omp"), stub)
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
	// A stdin that is not a terminal: omp would wait on it and read it as more
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
// (ADR 26-10-14-718 §5): judgeModel from Discobox's settings, else omp's
// default model role followed through its aliases, else omp's own pick.
func TestPromptJudgesWithTheConfiguredModel(t *testing.T) {
	for _, tc := range []struct {
		name, setting, roles, role, want string
	}{
		{"nothing configured is omp's own pick", "", "", "judge", ""},
		{"the default role", "", `{"default":"anthropic/claude-sonnet-5-5"}`, "judge", "--model=anthropic/claude-sonnet-5-5"},
		{"an alias is followed", "", `{"default":"@slow","slow":"openai-codex/gpt-6-astra:high"}`, "judge", "--model=openai-codex/gpt-6-astra:high"},
		{"an alias loop names nothing", "", `{"default":"@slow","slow":"@default"}`, "judge", ""},
		{"judgeModel wins", `{"judgeModel":"zai/glm-5.3"}`, `{"default":"anthropic/claude-sonnet-5-5"}`, "judge", "--model=zai/glm-5.3"},
		{"an empty judgeModel falls through", `{"judgeModel":""}`, `{"default":"anthropic/claude-sonnet-5-5"}`, "judge", "--model=anthropic/claude-sonnet-5-5"},
		{"a model that reads like a flag stays the model's value", `{"judgeModel":"--no-tools=false"}`, "", "judge", "--model=--no-tools=false"},
		{"fast is the smol role", "", `{"default":"a/big","smol":"a/small"}`, "fast", "--model=a/small"},
		{"fast with no smol role is the default", "", `{"default":"a/big"}`, "fast", "--model=a/big"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPromptRun(t, "ok", 0)
			if tc.setting != "" {
				p.writeHome(t, settingsPath, tc.setting)
			}
			if _, err := p.run(t, map[string]string{"ROLES": tc.roles}, "--model", tc.role, "--no-tools", "--prompt", "is it safe"); err != nil {
				t.Fatal(err)
			}
			var got string
			for _, arg := range p.args(t) {
				if strings.HasPrefix(arg, "--model") {
					got = arg
				}
			}
			if got != tc.want {
				t.Fatalf("model argument = %q, want %q (argv %q)", got, tc.want, p.args(t))
			}
		})
	}
}

// The roles are read where no project's settings answer.
func TestPromptReadsTheRolesOutsideTheProject(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	if _, err := p.run(t, map[string]string{"ROLES": `{"default":"a/b"}`}, "--model", "judge", "--prompt", "q"); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if got := strings.TrimSpace(p.read(t, "config-cwd")); got == cwd || !strings.HasPrefix(got, p.dir) {
		t.Fatalf("roles read from %s, want an empty directory of the wrapper's own", got)
	}
}

// TestPromptIsolatesTheToolFreeJudge covers what --no-tools shuts out: the
// caller is the agent being judged, so no settings, extension, instruction or
// variable it could have set may reach the judge — only the harness's
// credentials, imported into a home of the judge's own.
func TestPromptIsolatesTheToolFreeJudge(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	// The agent's credentials file: a key and a login the judge may use, and
	// entries that would each name the endpoint the judge's requests go to.
	p.writeHome(t, credentialsPath, `{"version":1,"credentials":[
		{"provider":"openrouter","credential":{"type":"api_key","key":"sk-or-v1-ab_cd","source":"login"}},
		{"provider":"anthropic","credential":{"type":"oauth","access":"sk-ant-oat01-ab_cd-EF","refresh":"r","expires":4102444800000,"accountId":"a-1","email":"me@example.com","region":"eu"}},
		{"provider":"github-copilot","credential":{"type":"oauth","access":"gho_x9","refresh":"gho_x9","expires":4102444800000,"apiEndpoint":"http://127.0.0.1:9999"}},
		{"provider":"snowflake","credential":{"type":"oauth","access":"eyJh_b.eyJj-d.s_ig","refresh":"r","expires":1,"enterpriseUrl":"http://127.0.0.1:9999"}},
		{"provider":"alibaba-token-plan","credential":{"type":"api_key","key":"{\"token\":\"S-AL\",\"baseUrl\":\"http://127.0.0.1:9999\"}"}},
		{"provider":"alibaba-token-plan","credential":{"type":"api_key","key":" {\"token\":\"S-AL\",\"baseUrl\":\"http://127.0.0.1:9999\"}"}},
		{"provider":"alibaba-token-plan","credential":{"type":"api_key","key":"\n{\"token\":\"S-AL\",\"baseUrl\":\"http://127.0.0.1:9999\"}"}},
		{"provider":"alibaba-coding-plan","credential":{"type":"oauth","access":" {\"baseUrl\":\"http://127.0.0.1:9999\"}","refresh":"r","expires":1}}
	]}`)
	if _, err := p.run(t, map[string]string{
		"PI_CONFIG_FILES":    "/agent/overlay.yml",
		"OMP_PROFILE":        "agent",
		"XDG_CONFIG_HOME":    "/agent-config",
		"ANTHROPIC_BASE_URL": "http://agent.example",
		"PI_PROXY":           "http://agent.example:1",
		"HTTPS_PROXY":        "http://127.0.0.1:17008",
	}, "--model", "judge", "--no-tools", "--system", "You judge.", "--prompt", "is it safe"); err != nil {
		t.Fatal(err)
	}
	args := p.args(t)
	for _, want := range []string{"-p", "--no-session", "--no-tools", "--no-extensions", "--no-skills", "--no-rules", "--no-lsp", "--system-prompt=You judge.\n"} {
		found := false
		for _, arg := range args {
			found = found || arg == want || (strings.HasSuffix(want, "\n") && arg == strings.TrimSuffix(want, "\n"))
		}
		if !found {
			t.Errorf("judge argv %q lacks %q", args, want)
		}
	}
	if args[len(args)-2] != "--" || args[len(args)-1] != "is it safe" {
		t.Errorf("judge argv %q, want the prompt last, after --", args)
	}
	home := p.read(t, "judge-home")
	if home == p.home || !strings.HasPrefix(home, p.dir) {
		t.Errorf("judge ran with HOME %s, want one of its own", home)
	}
	if got := p.read(t, "home-contents"); got != "imported.json\n" {
		t.Errorf("judge's home holds %q, want only the imported credentials", got)
	}
	// What reached the judge's store: tokens and account names, no endpoint,
	// and no key that is itself a document.
	want := `{"version":1,"credentials":[` +
		`{"provider":"openrouter","credential":{"type":"api_key","key":"sk-or-v1-ab_cd"}},` +
		`{"provider":"anthropic","credential":{"type":"oauth","access":"sk-ant-oat01-ab_cd-EF","refresh":"r","expires":4102444800000,"accountId":"a-1","email":"me@example.com"}},` +
		`{"provider":"github-copilot","credential":{"type":"oauth","access":"gho_x9","refresh":"gho_x9","expires":4102444800000}},` +
		`{"provider":"snowflake","credential":{"type":"oauth","access":"eyJh_b.eyJj-d.s_ig","refresh":"r","expires":1}}]}`
	if got := strings.TrimSpace(p.read(t, "imported")); got != want {
		t.Errorf("judge imported\n  %s\nwant\n  %s", got, want)
	}
	// An empty directory that is its own repository root, so omp's walk for
	// context files stops in it.
	if got := p.read(t, "cwd-contents"); got != ".git\n" {
		t.Errorf("judge's working directory holds %q, want only .git", got)
	}
	env := p.read(t, "env")
	for _, leaked := range []string{"PI_CONFIG_FILES=", "OMP_PROFILE=", "XDG_CONFIG_HOME=", "ANTHROPIC_BASE_URL="} {
		if strings.Contains(env, leaked) {
			t.Errorf("judge inherited %s", strings.TrimSuffix(leaked, "="))
		}
	}
	// omp's Anthropic transport reads its proxy from PI_PROXY alone, so the
	// judge's is the egress proxy every other request goes through.
	if !strings.Contains(env, "\nPI_PROXY=http://127.0.0.1:17008\n") {
		t.Errorf("judge's PI_PROXY is not the sandbox's HTTPS_PROXY")
	}
	if got := p.read(t, "stdin"); got != "" {
		t.Errorf("omp read %q from stdin, want /dev/null", got)
	}
	// And the home it made is gone afterwards.
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("judge's home %s outlived it", home)
	}
}

// The system text always reaches omp as text: a one-line value of its
// system-prompt flags is read as a file when one by that name exists.
func TestPromptSystemTextIsNeverAPath(t *testing.T) {
	p := newPromptRun(t, "ok", 0)
	if _, err := p.run(t, nil, "--model", "judge", "--system", "/etc/hostname", "--prompt", "q"); err != nil {
		t.Fatal(err)
	}
	raw := p.read(t, "args")
	if !strings.Contains(raw, "--append-system-prompt=/etc/hostname\n\n") {
		t.Fatalf("argv %q, want the system text with its newline", raw)
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

// An omp that fails is a wrapper that failed, not one that answered nothing.
func TestPromptFailsWhenOmpDoes(t *testing.T) {
	p := newPromptRun(t, `{"allow":true,"reason":"not the answer"}`, 1)
	out, err := p.run(t, nil, "--model", "judge", "--prompt", "judge this", "--no-tools", "--output-schema", `{"type":"object"}`)
	if err == nil {
		t.Fatalf("stdout = %q, want the failure to reach the caller", out)
	}
	if strings.Contains(out, "allow") {
		t.Fatalf("stdout = %q, want nothing that reads as a verdict", out)
	}
}
