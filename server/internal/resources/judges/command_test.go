package judges

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func commandAsk() services.CommandAsk {
	return services.CommandAsk{
		SandboxID: "sandbox-1", UseID: "use_abc",
		Command:  []string{"gh", "pr", "create", "--body-file", "-"},
		Stdin:    &judge.Input{Content: "Fixes the flaky test"},
		Reported: &judge.Reported{WorkingDirectory: "/src/repo"},
	}
}

// A command is put to the project's judge as a command job built from the
// live grant, with what the discobox sent beside it as evidence, and the
// verdict is on record as the judge's before the answer goes back
// (ADR 26-09-22-838 §3).
func TestACommandIsJudgedAndRecordedAsTheJudges(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	judgeSandbox := sandboxes.created[0]
	ready(t, appStore, judgeSandbox)
	fake := newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "that opens the pull request"})
	service.SetLeases(fake)
	asked := 0
	service.SetUses(approvedUses{asked: &asked, use: services.ApprovedUse{
		Purpose: "open a pull request in org/repo", Host: "api.github.com", Credential: "GitHub token", GrantID: "grant-1",
	}})

	answer, err := service.JudgeCommand(ctx, "pool-1", commandAsk())
	if err != nil {
		t.Fatalf("JudgeCommand() error = %v", err)
	}
	if !answer.Allow {
		t.Fatalf("answer = %+v, want the judge's allow", answer)
	}
	jobs := fake.asked()
	if len(jobs) != 1 {
		t.Fatalf("the judge was asked %d times, want once", len(jobs))
	}
	job := jobs[0]
	stdin, _ := job.Stdin.Get()
	reported, _ := job.Reported.Get()
	if string(job.Kind) != judge.KindCommand || job.Purpose != "open a pull request in org/repo" || job.Host != "api.github.com" ||
		len(job.Command) != 5 || stdin.Content != "Fixes the flaky test" || reported.WorkingDirectory.Or("") != "/src/repo" {
		t.Fatalf("job = %+v, want a command job from the grant carrying the discobox's evidence", job)
	}
	if !slices.Equal(job.Facts, workerFacts) {
		t.Fatalf("facts = %q, want what the control plane recorded about the discobox asking: %q", job.Facts, workerFacts)
	}
	// Once to build the question, once after the verdict.
	if asked != 2 {
		t.Fatalf("the use was resolved %d times, want it asked again after the verdict", asked)
	}

	verdicts, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %d, want the one command verdict", len(verdicts))
	}
	row := verdicts[0]
	if row.Kind != model.CredentialVerdictKindCommand || row.Origin != model.CredentialVerdictOriginJudge ||
		row.SandboxID != "sandbox-1" || row.UseID != "use_abc" || row.GrantID != "grant-1" || !row.Allow ||
		row.JudgeSandboxID != judgeSandbox.ID || row.PromptVersion != judge.PromptVersion || len(row.Command) != 5 {
		t.Fatalf("verdict = %+v, want the judge's command verdict against the grant", row)
	}
	var recorded judge.Job
	if err := json.Unmarshal([]byte(row.Prompt), &recorded); err != nil || recorded.Stdin == nil || recorded.Reported == nil {
		t.Fatalf("prompt = %q, want the job as the judge read it, input and report included", row.Prompt)
	}
}

// A judge that asks to be shown more has not allowed a command: there is
// nothing more to show.
func TestACommandTheJudgeAsksAboutIsNotAllowed(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready(t, appStore, sandboxes.created[0])
	service.SetLeases(newAnsweringJudge(t, sandboxapi.JudgeAnswer{
		Need: sandboxapi.NewOptJudgeNeed(sandboxapi.JudgeNeed{Body: true}), Reason: "show me",
	}))
	service.SetUses(approvedUses{})

	answer, err := service.JudgeCommand(ctx, "pool-1", commandAsk())
	if err != nil {
		t.Fatalf("JudgeCommand() error = %v", err)
	}
	if answer.Allow || answer.Need != nil {
		t.Fatalf("answer = %+v, want a refusal", answer)
	}
}

// Each switch answers for its own kind of job (ADR 26-10-02-054): a server
// that judges commands and not requests answers the one and says, in the way
// a pool recognizes, that it does not answer the other — and the other way
// round. Delegations go with commands.
func TestEachSwitchAnswersForItsOwnJobs(t *testing.T) {
	ctx := context.Background()
	judgingDisabled := func(err error) bool {
		var status apperrors.StatusError
		kind, ok := apperrors.KindOf(err)
		return errors.As(err, &status) && status.StatusCode() == http.StatusServiceUnavailable &&
			ok && kind == apperrors.KindJudgingDisabled
	}
	delegation := services.DelegationAsk{Delegated: []string{"read issues"}, Uses: []string{"read issue 43"}, Hosts: []string{"api.github.com"}}

	t.Run("commands only", func(t *testing.T) {
		service, appStore, _ := newJudgeTest(t)
		service.judging = Judging{Commands: true}
		defaultHarness(t, appStore, "codex", "sha256:one")
		service.SetUses(approvedUses{})
		if _, err := service.Judge(ctx, "pool-1", requestAsk()); !judgingDisabled(err) {
			t.Fatalf("Judge() error = %v, want judging disabled for requests", err)
		}
		if _, err := service.JudgeCommand(ctx, "pool-1", commandAsk()); err == nil || judgingDisabled(err) {
			t.Fatalf("JudgeCommand() error = %v, want it asked of the judge, which is not ready", err)
		}
	})
	t.Run("requests only", func(t *testing.T) {
		service, appStore, _ := newJudgeTest(t)
		service.judging = Judging{Requests: true}
		defaultHarness(t, appStore, "codex", "sha256:one")
		service.SetUses(approvedUses{})
		if _, err := service.JudgeCommand(ctx, "pool-1", commandAsk()); !judgingDisabled(err) {
			t.Fatalf("JudgeCommand() error = %v, want judging disabled for commands", err)
		}
		if _, err := service.JudgeDelegation(ctx, "project-1", delegation); !judgingDisabled(err) {
			t.Fatalf("JudgeDelegation() error = %v, want judging disabled with commands", err)
		}
	})
}

// Either switch is reason enough for a project to keep a judge.
func TestEitherSwitchKeepsAJudge(t *testing.T) {
	for name, judging := range map[string]Judging{"commands": {Commands: true}, "requests": {Requests: true}} {
		t.Run(name, func(t *testing.T) {
			service, appStore, sandboxes := newJudgeTest(t)
			service.judging = judging
			defaultHarness(t, appStore, "codex", "sha256:one")
			if _, err := service.Reconcile(context.Background(), "project-1"); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(sandboxes.created) != 1 {
				t.Fatalf("created %d discoboxes, want the judge", len(sandboxes.created))
			}
		})
	}
}

// A project that cannot have a judge cannot run a credentialed command, and
// the refusal says what fixes it, which is nothing the discobox can do
// (ADR 26-10-02-054 §2). A judge that is merely not up yet says nothing of
// the kind.
func TestACommandWithNoJudgeToAskSaysTheWayOut(t *testing.T) {
	ctx := context.Background()
	service, appStore, _ := newJudgeTest(t)
	defaultHarness(t, appStore, "shell", "sha256:one")
	service.SetLeases(refusingLeases{})
	service.SetUses(approvedUses{})
	_, err := service.JudgeCommand(ctx, "pool-1", commandAsk())
	if err == nil || !strings.Contains(err.Error(), "runs no model") || !strings.Contains(err.Error(), "judgeCommands: false") {
		t.Fatalf("JudgeCommand() error = %v, want why there is no judge and the way out", err)
	}

	service, appStore, _ = newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	service.SetLeases(refusingLeases{})
	service.SetUses(approvedUses{})
	_, err = service.JudgeCommand(ctx, "pool-1", commandAsk())
	if err == nil || strings.Contains(err.Error(), "judgeCommands") {
		t.Fatalf("JudgeCommand() error = %v, want a judge not ready refused without a remedy it does not need", err)
	}
}
