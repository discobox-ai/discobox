package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/x/shorttmp"
)

// newJudgeService is the project's judge: a discobox that answers judging asks
// from its pool and is worked in by nobody (ADR 0149 §1). wrapper stands in for
// the image's discobox-prompt, which the judge resolves on the harness
// environment's PATH.
func newJudgeService(t *testing.T, mode, wrapper string) *Service {
	t.Helper()
	dir := shorttmp.Dir(t)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "discobox-prompt"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	// The stub comes first on a PATH that is otherwise this machine's, as a
	// harness environment's is: the wrapper is a script, and a script needs
	// the ordinary commands.
	env := map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	units := &fakeUnits{}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         env,
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       execManager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         env,
		Harness:     config.Harness{ID: "claude-code", Command: []string{"/usr/local/bin/claude"}},
		HarnessMode: mode,
		Units:       units,
		Installer:   &noopInstaller{},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func requestJob() judge.Job {
	return judge.Job{
		Kind: judge.KindRequest, Purpose: "open a pull request in org/repo",
		Host: "api.github.com", Round: 1,
		Request: &judge.Request{Method: "POST", URL: "https://api.github.com/graphql"},
	}
}

// What the wrapper answers is what the judge answers, and the three answers a
// verdict may be all arrive intact.
func TestJudgeAnswersFromTheHarness(t *testing.T) {
	for _, tc := range []struct {
		name, said string
		want       judge.Answer
	}{
		{"an allow", `{"allow":true,"reason":"opening the PR it was approved for"}`,
			judge.Answer{Allow: true, Reason: "opening the PR it was approved for"}},
		{"a refusal", `{"allow":false,"reason":"deleting a repository is not opening a PR"}`,
			judge.Answer{Reason: "deleting a repository is not opening a PR"}},
		{"an ask for the body", `{"need":{"body":"json"},"reason":"the operation is in the body"}`,
			judge.Answer{Need: &judge.Need{Body: judge.FormJSON}, Reason: "the operation is in the body"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newJudgeService(t, config.HarnessModeJudge, "#!/bin/sh\nprintf '%s\\n' '"+tc.said+"'\n")
			answer, err := svc.Judge(context.Background(), requestJob())
			if err != nil {
				t.Fatalf("Judge() error = %v", err)
			}
			if answer.Allow != tc.want.Allow || answer.Reason != tc.want.Reason {
				t.Fatalf("answer = %+v, want %+v", answer, tc.want)
			}
			if (answer.Need == nil) != (tc.want.Need == nil) {
				t.Fatalf("need = %+v, want %+v", answer.Need, tc.want.Need)
			}
		})
	}
}

// The wrapper is asked the question Discobox asks, not one the caller composed:
// the role, the system prompt, the schema and the tools restriction are this
// process's, and the evidence arrives as the job's own JSON.
func TestJudgeAsksWithTheContractsOwnPrompt(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	svc := newJudgeService(t, config.HarnessModeJudge,
		"#!/bin/sh\nprintf '%s\\n' \"$@\" > "+args+"\nprintf '{\"allow\":true,\"reason\":\"fine\"}\\n'\n")

	if _, err := svc.Judge(context.Background(), requestJob()); err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	asked, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := string(asked)
	for _, want := range []string{
		"--model\n" + judge.Role + "\n",
		"--output-schema\n" + judge.Schema + "\n",
		"--no-tools\n",
		judge.System,
		`"purpose":"open a pull request in org/repo"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the wrapper was asked:\n%s\nwant it to carry %q", got, want)
		}
	}
}

// Only a judge judges. Every other discobox exists to be worked in, and a
// request that reached one is asking the wrong sandbox.
func TestJudgeRefusesADiscoboxThatIsNotOne(t *testing.T) {
	for _, mode := range []string{config.HarnessModeRun, config.HarnessModeConfig, ""} {
		svc := newJudgeService(t, mode, "#!/bin/sh\nprintf '{\"allow\":true,\"reason\":\"fine\"}\\n'\n")
		if _, err := svc.Judge(context.Background(), requestJob()); err == nil {
			t.Fatalf("harnessMode %q judged; only a judge runtime does", mode)
		}
	}
}

// An answer that is not a verdict is not a verdict, whatever it says. What the
// wrapper printed is not passed back: it is model output about a request a
// discobox wrote, and the caller refuses either way.
func TestJudgeRefusesAnAnswerThatIsNotAVerdict(t *testing.T) {
	for _, said := range []string{
		"I think that's fine!",
		`{"allow":true}`,
		`Sure: {"allow":true,"reason":"fine"}`,
		"",
	} {
		svc := newJudgeService(t, config.HarnessModeJudge, "#!/bin/sh\nprintf '%s\\n' '"+said+"'\n")
		answer, err := svc.Judge(context.Background(), requestJob())
		if err == nil {
			t.Fatalf("answer %q read as the verdict %+v", said, answer)
		}
		if answer.Allow {
			t.Fatalf("answer %q allowed while failing", said)
		}
		if strings.Contains(err.Error(), "fine") {
			t.Fatalf("error = %v, want it not to carry what the model said", err)
		}
	}
}

// A wrapper that fails is a judge that did not answer, and what it printed
// while failing does not travel with the refusal: the judge runs with the
// project's harness credential in its environment, and a CLI that cannot
// authenticate prints back what it tried.
func TestJudgeRefusesWhenTheWrapperFails(t *testing.T) {
	svc := newJudgeService(t, config.HarnessModeJudge,
		"#!/bin/sh\necho 'auth failed for sk-secret-value' >&2\nexit 7\n")
	_, err := svc.Judge(context.Background(), requestJob())
	if err == nil {
		t.Fatal("a failed wrapper answered")
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Fatalf("error = %v, want what the wrapper printed kept out of it", err)
	}
	var failure *execs.OnceFailure
	if !errors.As(err, &failure) || !strings.Contains(failure.Stderr, "sk-secret-value") {
		t.Fatalf("error = %v, want the detail reachable by a caller that has somewhere to put it", err)
	}
}

// One at a time: a second ask while the first is still being answered is told
// so, rather than queued behind it, because the caller is holding a request
// open and its deadline is what decides.
func TestJudgeAnswersOneAtATime(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	svc := newJudgeService(t, config.HarnessModeJudge,
		"#!/bin/sh\ntouch "+started+"\nsleep 2\nprintf '{\"allow\":true,\"reason\":\"fine\"}\\n'\n")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Judge(context.Background(), requestJob())
	}()
	t.Cleanup(func() { <-done })

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first ask never reached the wrapper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := svc.Judge(context.Background(), requestJob()); !Busy(err) {
		t.Fatalf("second ask error = %v, want it told the judge is already answering", err)
	}
}
