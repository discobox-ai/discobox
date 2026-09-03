package access

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
)

// The judge is what stands between an approved use and a command that is not
// it (ADR 0079). What these assert is the part an agent and an operator both
// depend on: a command that is not the use never starts, and neither does one
// the judge could not rule on.

// judgeCredentials is a service that lists one credential with one approved
// use, which is what `run` needs before it can judge anything.
func judgeCredentials() []agentcreds.Credential {
	return []agentcreds.Credential{{
		Name:   "github",
		EnvVar: "GITHUB_TOKEN",
		Host:   "api.github.com",
		Uses:   []agentcreds.Use{{UseID: "use_7f3c", Description: "Open a PR against the current repo"}},
	}}
}

// stubJudge installs a fake discobox-prompt whose body is the given shell
// script, and returns the file its arguments are recorded in. The CLI finds it
// through the documented override rather than a doctored PATH, which is also
// the path a caller outside a sandbox takes.
func stubJudge(t *testing.T, script string) (argsFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub judge is a shell script")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	path := filepath.Join(dir, "discobox-prompt")
	// NUL-separated: --system and --prompt are multi-line, so a line-separated
	// record could not be split back into arguments.
	body := "#!/bin/sh\nfor arg in \"$@\"; do printf '%s\\000' \"$arg\" >>" + argsFile + "; done\n" + script
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write stub judge: %v", err)
	}
	t.Setenv(PromptCommandEnv, path)
	return argsFile
}

const allowScript = `printf '{"allow":true,"reason":"opens a PR"}\n'`

func TestRunRefusesACommandTheJudgeDenies(t *testing.T) {
	stubJudge(t, `printf '{"allow":false,"reason":"deletes repositories, which is not the approved use"}\n'`)
	svc := &fakeService{credentials: judgeCredentials()}
	serve(t, svc)

	marker := filepath.Join(t.TempDir(), "ran")
	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--json", "--", "sh", "-c", "touch " + marker})
	})
	if code == exitOK {
		t.Fatal("a denied command exited 0")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran; a denied command must never start")
	}
	// No value may have been taken either: a refusal must not leave an
	// activation behind for a command that never ran.
	if svc.gotUse.UseID != "" {
		t.Fatalf("get was called with %#v; the judge runs first", svc.gotUse)
	}
	if !strings.Contains(stderr, agentcreds.CodeDenied) {
		t.Fatalf("stderr = %q, want the stable denied code an agent branches on", stderr)
	}
	if !strings.Contains(stderr, "not the approved use") {
		t.Fatalf("stderr = %q, want the judge's own reason", stderr)
	}
}

func TestRunAsksTheJudgeAboutTheApprovedUseAndTheRealArgv(t *testing.T) {
	argsFile := stubJudge(t, allowScript)
	serve(t, &fakeService{credentials: judgeCredentials()})

	_, _, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "exit 0"})
	})
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	recorded, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read recorded args: %v", err)
	}
	args := strings.Split(strings.TrimSuffix(string(recorded), "\x00"), "\x00")
	flags := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	if flags["--model"] != judgeModel {
		t.Fatalf("--model = %q, want the role %q; the CLI must not name a model", flags["--model"], judgeModel)
	}
	if flags["--output-schema"] != verdictSchema {
		t.Fatalf("--output-schema = %q, want the verdict schema", flags["--output-schema"])
	}
	if !strings.Contains(flags["--system"], "security judge") {
		t.Fatalf("--system = %q, want the judging instructions", flags["--system"])
	}
	noTools := false
	for _, arg := range args {
		if arg == "--no-tools" {
			noTools = true
		}
	}
	if !noTools {
		t.Fatalf("args = %v, want --no-tools; the judge must never be given tools (ADR 0090)", args)
	}
	prompt := flags["--prompt"]
	for _, want := range []string{"Open a PR against the current repo", "api.github.com", "GITHUB_TOKEN", "[0] sh", "[2] exit 0"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("--prompt = %q, want it to carry %q", prompt, want)
		}
	}
}

// An allowed command's verdict has to reach the same call that takes the
// value, not just decide whether that call happens (ADR 0091 §1).
func TestRunCarriesTheVerdictOnTheUseCall(t *testing.T) {
	stubJudge(t, allowScript)
	svc := &fakeService{credentials: judgeCredentials()}
	serve(t, svc)

	_, _, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "exit 0"})
	})
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !svc.gotUse.Verdict.Allow || svc.gotUse.Verdict.Reason != "opens a PR" {
		t.Fatalf("verdict on the use call = %#v, want the judge's own answer", svc.gotUse.Verdict)
	}
	if svc.gotUse.Verdict.Role != judgeModel || svc.gotUse.Verdict.Prompt == "" {
		t.Fatalf("verdict = %#v, want the role and the exact prompt carried too", svc.gotUse.Verdict)
	}
}

// A denial never reaches the use call, so it is reported on its own — the
// only route ADR 0091 §3 gives it to trusted ground. Reporting it must not
// change what the CLI itself reports for the refusal.
func TestRunReportsADenialTheUseCallNeverSees(t *testing.T) {
	stubJudge(t, `printf '{"allow":false,"reason":"deletes repositories, which is not the approved use"}\n'`)
	svc := &fakeService{credentials: judgeCredentials()}
	serve(t, svc)

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--json", "--", "sh", "-c", "exit 0"})
	})
	if code == exitOK {
		t.Fatal("a denied command ran")
	}
	if !strings.Contains(stderr, "deletes repositories") {
		t.Fatalf("stderr = %q, want the judge's own reason", stderr)
	}
	if svc.gotUse.UseID != "" {
		t.Fatal("the use call ran even though the judge denied the command")
	}
	if svc.gotDenial.Verdict.Allow || svc.gotDenial.Verdict.Reason == "" {
		t.Fatalf("reported denial = %#v, want the refused verdict", svc.gotDenial)
	}
	if svc.gotDenial.UseID != "use_7f3c" {
		t.Fatalf("reported denial useId = %q, want use_7f3c", svc.gotDenial.UseID)
	}
}

// Reporting is best-effort: its own failure must not surface as run's.
func TestRunIgnoresAFailureToReportADenial(t *testing.T) {
	stubJudge(t, `printf '{"allow":false,"reason":"not the approved use"}\n'`)
	svc := &fakeService{credentials: judgeCredentials(), denialErr: errors.New("control plane unreachable")}
	serve(t, svc)

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--json", "--", "sh", "-c", "exit 0"})
	})
	if code == exitOK {
		t.Fatal("a denied command ran")
	}
	if strings.Contains(stderr, "control plane unreachable") {
		t.Fatalf("stderr = %q, want the judge's own refusal, not the report's failure", stderr)
	}
}

// A refusal for want of a judge at all — no wrapper installed — never asked
// one anything, so there is nothing to report.
func TestRunReportsNothingWhenNoJudgeWasEverAsked(t *testing.T) {
	t.Setenv(PromptCommandEnv, "discobox-prompt-absent")
	svc := &fakeService{credentials: judgeCredentials()}
	serve(t, svc)

	capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "exit 0"})
	})
	if svc.gotDenial.UseID != "" {
		t.Fatalf("denial reported = %#v, want none: no judge was ever reached", svc.gotDenial)
	}
}

// Every way the gate can fail to produce a yes is a refusal. A judge that
// cannot answer must not become a judge that is skipped.
func TestRunRefusesWhenTheJudgeCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"exits non-zero", `printf 'model unreachable\n' >&2; exit 1`},
		{"answers nothing", `exit 0`},
		{"answers prose", `printf 'Sure, that looks fine to me!\n'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubJudge(t, tc.script)
			svc := &fakeService{credentials: judgeCredentials()}
			serve(t, svc)

			marker := filepath.Join(t.TempDir(), "ran")
			_, _, code := capture(t, "", func() int {
				return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "touch " + marker})
			})
			if code == exitOK {
				t.Fatal("the command ran without a verdict")
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the command ran; an unanswered judge must fail closed")
			}
			if svc.gotUse.UseID != "" {
				t.Fatal("a value was taken for a command that was never judged")
			}
		})
	}
}

// A harness that ships no wrapper is a configuration gap, and it must read like
// one rather than like a credential problem the agent can fix by asking again.
func TestRunRefusesWhenNoJudgeIsInstalled(t *testing.T) {
	t.Setenv(PromptCommandEnv, filepath.Join(t.TempDir(), "discobox-prompt-absent"))
	serve(t, &fakeService{credentials: judgeCredentials()})

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_7f3c", "--", "sh", "-c", "exit 0"})
	})
	if code == exitOK {
		t.Fatal("a command ran with no judge installed")
	}
	if !strings.Contains(stderr, "discobox-prompt-absent") {
		t.Fatalf("stderr = %q, want it to name the missing wrapper", stderr)
	}
	if !strings.Contains(stderr, PromptCommandEnv) {
		t.Fatalf("stderr = %q, want it to name the override", stderr)
	}
}

// A use the service does not list has no approved sentence to judge against,
// so it is refused before a model or the service is asked anything.
func TestRunRefusesAUseThatIsNotListed(t *testing.T) {
	stubJudge(t, allowScript)
	svc := &fakeService{credentials: judgeCredentials()}
	serve(t, svc)

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"run", "--use", "use_gone", "--json", "--", "sh", "-c", "exit 0"})
	})
	if code == exitOK {
		t.Fatal("an unlisted use ran a command")
	}
	if svc.gotUse.UseID != "" {
		t.Fatal("get was called for a use that could not be judged")
	}
	if !strings.Contains(stderr, agentcreds.CodeDenied) {
		t.Fatalf("stderr = %q, want the denied code", stderr)
	}
}

// Wrappers are shell scripts around harness CLIs, and some of those frame an
// answer in a transcript. The verdict is still the verdict.
func TestVerdictIsReadFromATranscript(t *testing.T) {
	answer, err := decodeVerdict([]byte("thinking...\n{\"allow\":true,\"reason\":\"matches\"}\ndone\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !answer.Allow || answer.Reason != "matches" {
		t.Fatalf("verdict = %#v", answer)
	}
	if _, err := decodeVerdict([]byte("no json here")); err == nil {
		t.Fatal("prose decoded as a verdict")
	}
}
