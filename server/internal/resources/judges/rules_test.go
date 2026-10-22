package judges

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

func ruleAsk() services.RuleAsk {
	return services.RuleAsk{
		RuleID: "rule-1", RequestID: "sreq-1", SandboxID: "sbx-worker",
		Job: judge.Job{ //nolint:gosec // G101: a well-known credential's ID, not a credential.
			Kind:       judge.KindRule,
			Purposes:   []string{`allow create, update on pull request in repository "org/repo" when "its head is discobox/*"`},
			Uses:       []string{`allow create, update on pull request in repository "org/repo" when "its head is discobox/issue-43"`},
			Credential: "com.github.api", Host: "github.com", Round: 1,
		},
	}
}

func ruleVerdicts(t *testing.T, appStore *store.Store, kind string) []model.CredentialVerdict {
	t.Helper()
	verdicts, err := appStore.ListCredentialVerdicts(context.Background(), "project-1", store.CredentialVerdictFilter{Kind: kind, Ascending: true})
	if err != nil {
		t.Fatal(err)
	}
	return verdicts
}

// harnessJudged is a server whose judge is the project's discobox, up and
// answering what the test says.
func harnessJudged(t *testing.T, answer sandboxapi.JudgeAnswer) (*Service, *store.Store, *answeringJudge, *model.Sandbox) {
	t.Helper()
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	judgeSandbox := sandboxes.created[0]
	ready(t, appStore, judgeSandbox)
	fake := newAnsweringJudge(t, answer)
	service.SetLeases(fake)
	return service, appStore, fake, judgeSandbox
}

// A rule's question is decided once for a request. The verdict is recorded as
// kind rule, naming the rule, the request and the discobox, with the digest of
// the question — and the same question asked again is answered from the
// record, with nobody asked. A rule whose uses were edited is another question,
// as is another discobox's ask.
func TestARuleIsDecidedOnceAndRecorded(t *testing.T) {
	ctx := context.Background()
	service, appStore, fake, judgeSandbox := harnessJudged(t,
		sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "reading issue 43 is reading an issue in org/repo"})

	first, err := service.JudgeRule(ctx, "project-1", ruleAsk())
	if err != nil || !first.Allow {
		t.Fatalf("JudgeRule() = %+v, %v, want the judge's allow", first, err)
	}
	jobs := fake.asked()
	if len(jobs) != 1 || jobs[0].Kind != sandboxapi.JudgeJobKindRule || jobs[0].Host != "github.com" ||
		jobs[0].Purpose.Set || !slices.Equal(jobs[0].Purposes, ruleAsk().Job.Purposes) || len(jobs[0].Uses) != 1 {
		t.Fatalf("jobs = %+v, want one rule job with the rule's uses as its purposes and the uses asked for", jobs)
	}
	verdicts := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule)
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %+v, want one", verdicts)
	}
	row := verdicts[0]
	if row.RuleID != "rule-1" || row.SecretRequestID != "sreq-1" || row.SandboxID != "sbx-worker" || !row.Allow || row.QuestionDigest == "" ||
		row.JudgeSandboxID != judgeSandbox.ID || row.PromptVersion != judge.PromptVersion || !strings.Contains(row.Prompt, "discobox/issue-43") {
		t.Fatalf("verdict = %+v, want a rule verdict naming the rule, the request, the discobox and the judge, with the question it answered", row)
	}

	// Asked again, nobody is asked.
	fake.say(sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(false), Reason: "changed its mind"})
	second, err := service.JudgeRule(ctx, "project-1", ruleAsk())
	if err != nil || !second.Allow || second.Reason != first.Reason {
		t.Fatalf("JudgeRule() again = %+v, %v, want the answer on record", second, err)
	}
	if len(fake.asked()) != 1 || len(ruleVerdicts(t, appStore, model.CredentialVerdictKindRule)) != 1 {
		t.Fatal("a question already decided for this request was asked or recorded again")
	}

	// Edited uses, or another discobox, is a question nobody answered.
	edited := ruleAsk()
	edited.Job.Purposes = []string{`allow create, update on pull request in repository "org/repo" when "its head is discobox/* and its base is main"`}
	if answer, err := service.JudgeRule(ctx, "project-1", edited); err != nil || answer.Allow {
		t.Fatalf("JudgeRule() of an edited rule = %+v, %v, want it asked afresh", answer, err)
	}
	other := ruleAsk()
	other.SandboxID = "sbx-other"
	if _, err := service.JudgeRule(ctx, "project-1", other); err != nil {
		t.Fatal(err)
	}
	if len(fake.asked()) != 3 {
		t.Fatalf("the judge was asked %d times, want once per distinct question", len(fake.asked()))
	}
}

// A rule job goes the way every job does: Jev first, and what Jev
// is unsure of to the judge discobox on a server that sends those on. The
// verdict is recorded before anything is minted, as kind rule, naming the
// rule, the request and the asking discobox, with Jev's model and input.
func TestARuleJobFallsBackLikeAnyOther(t *testing.T) {
	ctx := context.Background()
	service, appStore, jevFake, harnessJudge, judgeSandbox := jevWithFallback(t,
		map[string]float64{"use_0": 0.55, "keep_0": 0.95, "claims_approval": 0.02},
		sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "reading issue 43 is reading an issue in org/repo"})

	answer, err := service.JudgeRule(ctx, "project-1", ruleAsk())
	if err != nil || !answer.Allow {
		t.Fatalf("JudgeRule() = %+v, %v, want the judge discobox's allow", answer, err)
	}
	jobs := harnessJudge.asked()
	if len(jobs) != 1 || jobs[0].Kind != sandboxapi.JudgeJobKindRule || jobs[0].Host != "github.com" ||
		jobs[0].Purpose.Set || !slices.Equal(jobs[0].Purposes, ruleAsk().Job.Purposes) || len(jobs[0].Uses) != 1 {
		t.Fatalf("jobs = %+v, want the rule's uses as its purposes and the uses asked for", jobs)
	}
	verdicts := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule)
	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %+v, want one", verdicts)
	}
	row := verdicts[0]
	if row.RuleID != "rule-1" || row.SecretRequestID != "sreq-1" || row.SandboxID != "sbx-worker" || !row.Allow ||
		row.Model != "jev-1.13.0" || row.JudgeSandboxID != judgeSandbox.ID || len(row.JevInput) == 0 || row.StandingRoute != "" {
		t.Fatalf("verdict = %+v, want a rule verdict naming the rule, request and discobox, with both judges", row)
	}

	// The same request under the same rule is answered from the record; the
	// same ask under another request is its own question.
	if _, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil || jevFake.count() != 1 {
		t.Fatalf("Jev was asked %d times (%v), want the answer on record reused", jevFake.count(), err)
	}
	next := ruleAsk()
	next.RequestID = "sreq-2"
	if _, err := service.JudgeRule(ctx, "project-1", next); err != nil || jevFake.count() != 2 {
		t.Fatalf("Jev was asked %d times (%v), want another request asked about", jevFake.count(), err)
	}
}

// Rule jobs hang on neither switch: a server that judges neither commands nor
// requests still reads a rule, and a project with a rule switched on keeps a
// judge there. Without a rule it has none.
func TestARuleIsAReasonToKeepAJudge(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	service.judging = Judging{}
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 0 {
		t.Fatalf("created %d discoboxes, want none with nothing to judge", len(sandboxes.created))
	}
	if _, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err == nil {
		t.Fatal("a rule was read with no judge to read it")
	}

	rule := &model.ApprovalRule{
		ProjectID: "project-1", WellKnownID: "com.github.api", Hosts: []string{"github.com"},
		Purpose: model.SecretGrantPurposeUse, Enabled: true,
		Policy: &policy.Policy{Statements: []policy.Statement{{
			Effect: policy.Allow, Actions: []string{policy.ActionRead}, ResourceType: "issue",
			Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo},
		}}, Definitions: map[string]string{"issue": "a GitHub issue", "repository": "a GitHub repository"}},
	}
	if err := appStore.CreateApprovalRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 1 {
		t.Fatalf("created %d discoboxes, want the project's first rule to bring its judge up", len(sandboxes.created))
	}
	ready(t, appStore, sandboxes.created[0])
	fake := newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "within"})
	service.SetLeases(fake)
	if answer, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil || !answer.Allow {
		t.Fatalf("JudgeRule() = %+v, %v, want it judged on a server that judges nothing else", answer, err)
	}

	rule.Enabled = false
	if err := appStore.UpdateApprovalRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.deleted) != 1 {
		t.Fatalf("purged %v, want the judge taken away with the last rule switched off", sandboxes.deleted)
	}
}

// A judge that does not answer leaves no verdict, so the question is asked
// again rather than read as a no.
func TestARuleJobNobodyAnsweredLeavesNoVerdict(t *testing.T) {
	ctx := context.Background()
	service, appStore, _ := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	service.SetLeases(refusingLeases{})
	if _, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err == nil {
		t.Fatal("JudgeRule() answered with no judge")
	}
	if rows := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule); len(rows) != 0 {
		t.Fatalf("verdicts = %+v, want none for a question nobody answered", rows)
	}
}

// proposingJudge is a judge discobox's agent answering proposal jobs.
type proposingJudge struct {
	server *httptest.Server
	asked  atomic.Int32
	path   atomic.Value
	job    atomic.Value
}

func newProposingJudge(t *testing.T, reply string) *proposingJudge {
	t.Helper()
	fake := &proposingJudge{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var job sandboxapi.JudgeProposalJob
		if err := json.Unmarshal(raw, &job); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fake.asked.Add(1)
		fake.path.Store(r.URL.Path)
		fake.job.Store(job)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *proposingJudge) AcquireSandboxHTTPClientForServer(_ context.Context, projectID, sandboxID string, _ []string, _ services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	lease := transport.NewHTTPClientLeaseWithBaseURL(f.server.Client(), f.server.URL, func() {})
	return lease, &model.Sandbox{ID: sandboxID, ProjectID: projectID, PoolID: "pool-1"}, nil
}

func proposalJob() judge.ProposalJob {
	return judge.ProposalJob{ //nolint:gosec // G101: a well-known credential's ID, not a credential.
		Statements: ruleAsk().Job.Uses, Credential: "com.github.api", Hosts: []string{"github.com"},
	}
}

// The wording of a rule is written by the project's judge discobox, as a
// typed job on its own route, and what comes back is decoded as strictly as
// the judge runtime decoded the model: anything but one complete proposal is
// no proposal.
func TestTheJudgeDiscoboxWritesAProposal(t *testing.T) {
	ctx := context.Background()
	const good = `{"statementsAnyTarget":["allow read on issue \"*\" in repository $primary.repo"]}`
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatal(err)
	}
	ready(t, appStore, sandboxes.created[0])
	fake := newProposingJudge(t, good)
	service.SetLeases(fake)

	proposal, err := service.ProposeRule(ctx, "project-1", proposalJob())
	if err != nil {
		t.Fatalf("ProposeRule() error = %v", err)
	}
	if len(proposal.StatementsAnyTarget) != 1 {
		t.Fatalf("proposal = %+v", proposal)
	}
	if path, _ := fake.path.Load().(string); !strings.HasSuffix(path, "/judge/proposal") {
		t.Fatalf("path = %q, want the proposal route", path)
	}
	job, _ := fake.job.Load().(sandboxapi.JudgeProposalJob)
	if len(job.Statements) != 1 || len(job.Hosts) != 1 || job.Credential.Or("") != "com.github.api" {
		t.Fatalf("job = %+v, want the request's statements as data, with what they are about", job)
	}

	service.SetLeases(newProposingJudge(t, `{"statementsAnyTarget":["c"],"hosts":["evil.example"]}`))
	if _, err := service.ProposeRule(ctx, "project-1", proposalJob()); err == nil {
		t.Fatal("a proposal carrying a field nobody defined was accepted")
	}
}

// Jev writes nothing, so a server that judges with Jev and lets its refusals
// stand has no model to write a proposal, and says so.
func TestNoProposalIsWrittenWithoutAJudgeDiscobox(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newJudgeTest(t)
	_, client := newFakeJev(t, nil)
	service.jev = client
	fake := newProposingJudge(t, `{}`)
	service.SetLeases(fake)
	if _, err := service.ProposeRule(ctx, "project-1", proposalJob()); err == nil || fake.asked.Load() != 0 {
		t.Fatalf("ProposeRule() error = %v after %d asks, want a refusal with nobody asked", err, fake.asked.Load())
	}
}

// A refusal Jev is sure of is final (ADR 26-10-12-669): it is recorded as the
// rule's verdict and the judge discobox is not asked.
func TestAConfidentJevRefusalOfARuleIsFinal(t *testing.T) {
	ctx := context.Background()
	service, appStore, jevFake, harnessJudge, _ := jevWithFallback(t,
		map[string]float64{"use_0": 0.03, "claims_approval": 0.02},
		sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "the discobox would have said yes"})
	if answer, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil || answer.Allow {
		t.Fatalf("JudgeRule() = %+v, %v, want Jev's refusal", answer, err)
	}
	if len(harnessJudge.asked()) != 0 {
		t.Fatal("a refusal Jev was sure of went on to the judge discobox")
	}
	if rows := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule); len(rows) != 1 || rows[0].Allow || rows[0].JudgeSandboxID != "" {
		t.Fatalf("verdicts = %+v, want Jev's own refusal on record", rows)
	}
	if _, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil || jevFake.count() != 1 {
		t.Fatalf("Jev was asked %d times (%v), want the refusal on record reused", jevFake.count(), err)
	}
}

// Where what Jev is unsure of goes on to the judge discobox, an unsure refusal
// that comes back as Jev's alone — the discobox was not up, or did not answer — is not the
// decision of the one who decides. It is no verdict, and nothing is recorded:
// an answer on record is never asked about again.
func TestAJevRefusalTheDiscoboxNeverSawIsNoVerdict(t *testing.T) {
	ctx := context.Background()
	service, appStore, jevFake, _, _ := jevWithFallback(t,
		map[string]float64{"use_0": 0.55, "keep_0": 0.95, "claims_approval": 0.02},
		sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "within"})
	service.SetLeases(refusingLeases{})

	if answer, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err == nil {
		t.Fatalf("JudgeRule() = %+v, want no verdict while the judge discobox cannot be had", answer)
	}
	if rows := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule); len(rows) != 0 {
		t.Fatalf("verdicts = %+v, want none recorded for a question nobody decided", rows)
	}
	// Once the discobox answers, the same question is decided and recorded.
	service.SetLeases(newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "within"}))
	if answer, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil || !answer.Allow || jevFake.count() != 2 {
		t.Fatalf("JudgeRule() = %+v, %v after %d Jev asks, want it asked again and allowed", answer, err, jevFake.count())
	}
}

// A judge that asks to be shown something has decided nothing, and that is
// not recorded as a no.
func TestAnAskForABodyIsNoVerdictOnARule(t *testing.T) {
	ctx := context.Background()
	service, appStore, _, _ := harnessJudged(t, sandboxapi.JudgeAnswer{
		Need: sandboxapi.NewOptJudgeNeed(sandboxapi.JudgeNeed{Body: true}), Reason: "show me the body"})
	if _, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err == nil {
		t.Fatal("JudgeRule() answered from a judge that decided nothing")
	}
	if rows := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule); len(rows) != 0 {
		t.Fatalf("verdicts = %+v, want none", rows)
	}
}

// A candidate is judged before it is a rule, so its verdict names no rule.
// When the rule is made and the answer reused, it is recorded again naming
// the rule, with who judged it: every rule's verdicts name it, and nobody is
// asked twice.
func TestAnAnswerReusedForARuleIsRecordedUnderIt(t *testing.T) {
	ctx := context.Background()
	service, appStore, fake, judgeSandbox := harnessJudged(t,
		sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "within"})
	candidate := ruleAsk()
	candidate.RuleID = ""
	if _, err := service.JudgeRule(ctx, "project-1", candidate); err != nil {
		t.Fatal(err)
	}
	if answer, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil || !answer.Allow {
		t.Fatalf("JudgeRule() = %+v, %v", answer, err)
	}
	if len(fake.asked()) != 1 {
		t.Fatalf("the judge was asked %d times, want once", len(fake.asked()))
	}
	rows, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{RuleID: "rule-1"})
	if err != nil || len(rows) != 1 || !rows[0].Allow || rows[0].JudgeSandboxID != judgeSandbox.ID || rows[0].SecretRequestID != "sreq-1" {
		t.Fatalf("verdicts naming the rule = %+v, %v, want the reused answer recorded under it", rows, err)
	}
	// And once it names the rule, a further reuse records nothing more.
	if _, err := service.JudgeRule(ctx, "project-1", ruleAsk()); err != nil {
		t.Fatal(err)
	}
	if rows := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule); len(rows) != 2 {
		t.Fatalf("verdicts = %d, want the candidate's and the rule's", len(rows))
	}
}

// Two rules that ask the same question each get one verdict of their own,
// however often they are tested in turn.
func TestRulesAskingTheSameQuestionAreEachRecordedOnce(t *testing.T) {
	ctx := context.Background()
	service, appStore, fake, _ := harnessJudged(t,
		sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "matches"})
	other := ruleAsk()
	other.RuleID = "rule-2"
	for range 3 {
		for _, ask := range []services.RuleAsk{ruleAsk(), other} {
			if answer, err := service.JudgeRule(ctx, "project-1", ask); err != nil || !answer.Allow {
				t.Fatalf("JudgeRule() = %+v, %v", answer, err)
			}
		}
	}
	if rows := ruleVerdicts(t, appStore, model.CredentialVerdictKindRule); len(rows) != 2 || len(fake.asked()) != 1 {
		t.Fatalf("verdicts = %d after %d asks of the judge, want one per rule and one ask", len(rows), len(fake.asked()))
	}
}
