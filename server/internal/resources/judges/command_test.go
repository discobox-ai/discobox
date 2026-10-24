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
	"github.com/discobox-ai/discobox/pkg/judge"
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
	if string(job.Kind) != judge.KindCommand || job.Purpose.Or("") != "open a pull request in org/repo" || job.Host != "api.github.com" ||
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

// A command spending the discobox API's credential names the discoboxes it
// acts on in its arguments — a positional ID, a flag's value, a segment of an
// API path, in either spelling of an ID — and is judged knowing what was
// recorded about each when it was created, beside the one asking: who created
// it is what a use bounded to "the discoboxes this discobox created" is read
// by (ADR 26-10-14-320 §5). An ID is a discobox's only identity, so a name, a
// short ID, an ID of no discobox in the project, and an ID in a command under
// another credential's use state nothing: the judge is told a discobox no
// fact describes has no creator it can rely on (judge.System).
func TestACommandStatesTheDiscoboxesItNames(t *testing.T) {
	ctx := context.Background()
	service, appStore, fake := gateJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "its own worker"})
	const child, persons, third, missing = "sbx_0123456789abcdef", "sbx_fedcba9876543210", "sbx_0123zzzzzzzzzzzz", "sbx_00000000000000zz"
	for _, sandbox := range []*model.Sandbox{
		{ID: child, ProjectID: "project-1", PoolID: "pool-1", Name: "child", CreatedBySandboxID: new("sbx-lead")},
		{ID: persons, ProjectID: "project-1", PoolID: "pool-1", Name: "persons"},
		{ID: third, ProjectID: "project-1", PoolID: "pool-1", Name: "third", CreatedBySandboxID: new("sbx-lead")},
	} {
		if err := appStore.CreateSandbox(ctx, sandbox); err != nil {
			t.Fatalf("create sandbox %s: %v", sandbox.ID, err)
		}
	}
	leadFacts := []string{
		"The discobox asking is sbx-lead.",
		"sbx-lead was created with no source.",
	}
	childFacts := []string{
		"A discobox the command names by its ID is " + child + ".",
		child + " was created with no source.",
		child + " was created by the discobox sbx-lead.",
	}
	personsFacts := []string{
		"A discobox the command names by its ID is " + persons + ".",
		persons + " was created with no source.",
		persons + " was created by no discobox.",
	}
	gate := services.ApprovedUse{
		Purpose: "look after the discoboxes this discobox created", Host: "api.discobox.internal", Credential: "ai.discobox.sandbox", GrantID: "grant-1",
	}
	github := services.ApprovedUse{Purpose: "open a pull request in org/repo", Host: "api.github.com", Credential: "GitHub token", GrantID: "grant-1"}
	for _, tc := range []struct {
		name    string
		use     services.ApprovedUse
		command []string
		want    []string
	}{
		{"by its ID", gate, []string{"discobox", "admin", "box", "get", child}, slices.Concat(leadFacts, childFacts)},
		{"by a flag", gate, []string{"discobox", "admin", "terminal", "screen", "primary", "--discobox-id", child}, slices.Concat(leadFacts, childFacts)},
		{"by a flag's value", gate, []string{"discobox", "admin", "terminal", "screen", "primary", "--discobox-id=" + child}, slices.Concat(leadFacts, childFacts)},
		{"in an API path", gate, []string{"discobox", "admin", "api", "-X", "POST", "/projects/default/sandboxes/" + persons + "/stop"}, slices.Concat(leadFacts, personsFacts)},
		{"as its hostname spells it", gate, []string{"discobox", "admin", "box", "delete", "sbx-fedcba9876543210"}, slices.Concat(leadFacts, personsFacts)},
		{"two, each once, in either spelling", gate, []string{"discobox", "rm", persons, child, "sbx-fedcba9876543210", persons}, slices.Concat(leadFacts, personsFacts, childFacts)},
		{"by a short ID", gate, []string{"discobox", "admin", "box", "get", "sbx-fedc"}, leadFacts},
		{"one not in the project", gate, []string{"discobox", "admin", "box", "get", missing}, leadFacts},
		{"by its name", gate, []string{"discobox", "admin", "box", "get", "persons"}, leadFacts},
		{"by its name, beside an ID", gate, []string{"discobox", "tag", "persons", "note=" + child}, slices.Concat(leadFacts, childFacts)},
		{"by the random part alone", gate, []string{"discobox", "admin", "box", "get", "fedcba9876543210"}, leadFacts},
		{"none", gate, []string{"discobox", "admin", "box", "ls"}, leadFacts},
		{"under another credential's use", github, []string{"gh", "pr", "create", "--title", child}, leadFacts},
		// More than fit are not left out unremarked: the judge reads them as
		// named and undescribed.
		{"more than fit", gate, []string{"discobox", "rm", child, persons, third}, slices.Concat(leadFacts, childFacts,
			[]string{"The command names 2 more discoboxes by their IDs than are described here."})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service.SetUses(approvedUses{use: tc.use})
			asked := len(fake.asked())
			if _, err := service.JudgeCommand(ctx, "pool-1", services.CommandAsk{SandboxID: "sbx-lead", UseID: "use_abc", Command: tc.command}); err != nil {
				t.Fatalf("JudgeCommand() error = %v", err)
			}
			jobs := fake.asked()
			if len(jobs) != asked+1 {
				t.Fatalf("the judge was asked %d times, want once more", len(jobs)-asked)
			}
			if got := jobs[len(jobs)-1].Facts; !slices.Equal(got, tc.want) {
				t.Fatalf("facts =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
