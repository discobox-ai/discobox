package judges

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/judge/jev"
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// pullRequestPolicy allows opening pull requests in this discobox's
// repository, as approved for sandbox-1, whose primary is org/repo.
func pullRequestPolicy(approvedRepo string) *policy.Policy {
	return &policy.Policy{
		Statements: []policy.Statement{{
			Effect: policy.Allow, Actions: []string{"create"}, ResourceType: "pull request",
			Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo},
		}},
		Definitions: map[string]string{"pull request": "A GitHub pull request.", "repository": "A GitHub repository."},
		Resolved:    map[string]string{policy.VarPrimaryRepo: approvedRepo},
	}
}

func policyUse(p *policy.Policy) services.ApprovedUse {
	return services.ApprovedUse{
		Purpose: "a sentence nobody reads once a policy is approved", Policy: p,
		Host: "api.github.com", Credential: "GitHub token", GrantID: "grant-1",
	}
}

var jevSureOfThePullRequest = map[string]float64{
	"s1_action": 0.95, "s1_resource": 0.96, "s1_scope": 0.94, "supporting": 0.1, "claims_approval": 0.01,
}

// A grant that carries a policy is judged by it (ADR 26-10-20-782 §8): Jev is
// asked about each statement with its variables bound to the discobox asking,
// code decides, and the verdict records how.
func TestARequestUnderAPolicyIsJudgedByIt(t *testing.T) {
	ctx := context.Background()
	service, appStore, _ := newJudgeTest(t)
	fake, client := newFakeJev(t, jevSureOfThePullRequest)
	service.jev = client
	service.SetUses(approvedUses{use: policyUse(pullRequestPolicy("org/repo"))})

	answer, err := service.Judge(ctx, "pool-1", requestAsk())
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if !answer.Allow || !strings.Contains(answer.Reason, "statement 1") {
		t.Fatalf("answer = %+v, want allowed by statement 1", answer)
	}
	sent := string(fake.sent())
	if !strings.Contains(sent, `\"org/repo\" (this discobox's primary repository)`) || strings.Contains(sent, "nobody reads") {
		t.Fatalf("Jev was sent %s, want the policy bound to org/repo and not the use's sentence", sent)
	}
	verdicts, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("recorded %d verdicts, want one", len(verdicts))
	}
	v := verdicts[0]
	if v.PromptVersion != jev.PolicyQuestionsVersion || v.PolicyDecision == nil || v.PolicyDecision.Statement != 1 ||
		v.PolicyDecision.Verdict != "allowed" || len(v.PolicyDecision.Answers) != len(jevSureOfThePullRequest) {
		t.Fatalf("verdict = %+v, decision = %+v, want Jev's policy questions and the statement that decided", v, v.PolicyDecision)
	}
	var asked judge.Job
	if err := json.Unmarshal([]byte(v.Prompt), &asked); err != nil || asked.Policy == nil || asked.Variables[policy.VarPrimaryRepo] != "org/repo" || asked.Purpose != "" {
		t.Fatalf("prompt = %s, want the job as put, with its policy and bound variables", v.Prompt)
	}
}

// A variable that resolves now to something other than what was approved
// matches nothing, so its statement is not even asked about.
func TestAPolicyWhoseVariableMovedAllowsNothing(t *testing.T) {
	ctx := context.Background()
	service, _, _ := newJudgeTest(t)
	fake, client := newFakeJev(t, jevSureOfThePullRequest)
	service.jev = client
	service.SetUses(approvedUses{use: policyUse(pullRequestPolicy("org/elsewhere"))})

	answer, err := service.Judge(ctx, "pool-1", requestAsk())
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if answer.Allow || strings.Contains(string(fake.sent()), "s1_") {
		t.Fatalf("answer = %+v, Jev sent %s; want a refusal with no question about statement 1", answer, fake.sent())
	}
}

// policyJudge is a judge discobox's runtime that answers judge/policy.
type policyJudge struct {
	server *httptest.Server
	mu     sync.Mutex
	paths  []string
	jobs   []sandboxapi.JudgeJob
	reply  sandboxapi.JudgePolicyReply
}

func newPolicyJudge(t *testing.T, reply sandboxapi.JudgePolicyReply) *policyJudge {
	t.Helper()
	fake := &policyJudge{reply: reply}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var job sandboxapi.JudgeJob
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		fake.paths = append(fake.paths, r.URL.Path)
		fake.jobs = append(fake.jobs, job)
		reply := fake.reply
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&reply)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *policyJudge) AcquireSandboxHTTPClientForServer(_ context.Context, projectID, sandboxID string, _ []string, _ services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	lease := transport.NewHTTPClientLeaseWithBaseURL(f.server.Client(), f.server.URL, func() {})
	return lease, &model.Sandbox{ID: sandboxID, ProjectID: projectID, PoolID: "pool-1"}, nil
}

// What Jev is unsure of goes to the judge discobox at its policy route, with
// the policy on the wire; it answers the questions, and this side combines
// them (ADR 26-10-20-782 §8).
func TestWhatJevIsUnsureOfUnderAPolicyGoesToTheJudgeDiscoboxsAnswers(t *testing.T) {
	ctx := context.Background()
	unsure := map[string]float64{}
	for id, p := range jevSureOfThePullRequest {
		unsure[id] = p
	}
	unsure["s1_scope"] = 0.5
	service, appStore, fake, _, judgeSandbox := jevWithFallback(t, unsure, sandboxapi.JudgeAnswer{})
	answers := []sandboxapi.JudgePolicyAnswer{}
	for _, id := range []string{"s1_action", "s1_resource", "s1_scope", "supporting", "claims_approval"} {
		said := sandboxapi.JudgePolicyAnswerAnswerYes
		if id == "supporting" || id == "claims_approval" {
			said = sandboxapi.JudgePolicyAnswerAnswerNo
		}
		answers = append(answers, sandboxapi.JudgePolicyAnswer{ID: id, Answer: said, Reason: "it is"})
	}
	harness := newPolicyJudge(t, sandboxapi.JudgePolicyReply{Answers: answers, PromptVersion: "policy-runtime", QuestionsDigest: pullRequestDigest(t)})
	service.SetLeases(harness)
	service.SetUses(approvedUses{use: policyUse(pullRequestPolicy("org/repo"))})

	answer, err := service.Judge(ctx, "pool-1", requestAsk())
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if !answer.Allow || fake.count() != 1 || len(harness.paths) != 1 || !strings.HasSuffix(harness.paths[0], "/judge/policy") {
		t.Fatalf("answer = %+v, Jev asked %d, judge discobox at %v; want the judge discobox's answers allowing", answer, fake.count(), harness.paths)
	}
	job := harness.jobs[0]
	sentPolicy, ok := job.Policy.Get()
	variables, _ := job.Variables.Get()
	if !ok || len(sentPolicy.Statements) != 1 || variables[policy.VarPrimaryRepo] != "org/repo" || job.Purpose.Set {
		t.Fatalf("job = %+v, want the policy and its bound variables, and no purpose", job)
	}
	verdicts, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{})
	if err != nil {
		t.Fatal(err)
	}
	v := verdicts[0]
	if v.JudgeSandboxID != judgeSandbox.ID || v.PromptVersion != "policy-runtime" || v.PolicyDecision == nil ||
		v.PolicyDecision.Answers[0].Reason != "it is" || v.Probabilities["s1_scope"] != 0.5 {
		t.Fatalf("verdict = %+v, want the judge discobox's decision beside Jev's probabilities", v)
	}
}

// pullRequestDigest is the digest of the questions this server asks about
// requestAsk under pullRequestPolicy, approved for org/repo.
func pullRequestDigest(t *testing.T) string {
	t.Helper()
	questions, err := judge.PolicyQuestions(judge.Job{
		Kind: judge.KindRequest, Host: "api.github.com", Round: 1, Request: requestAsk().Request,
		Policy: pullRequestPolicy("org/repo"), Variables: map[string]string{policy.VarPrimaryRepo: "org/repo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return judge.QuestionsDigest(questions)
}

// A runtime that asked other questions than this server would is no verdict:
// its answers are under IDs whose words this side cannot vouch for.
func TestAJudgeDiscoboxThatAskedOtherQuestionsIsNoVerdict(t *testing.T) {
	ctx := context.Background()
	unsure := map[string]float64{}
	for id, p := range jevSureOfThePullRequest {
		unsure[id] = p
	}
	unsure["s1_scope"] = 0.5
	service, _, _, _, _ := jevWithFallback(t, unsure, sandboxapi.JudgeAnswer{})
	// Answers that would allow, so only the digest can refuse.
	answers := []sandboxapi.JudgePolicyAnswer{}
	for _, id := range []string{"s1_action", "s1_resource", "s1_scope", "supporting", "claims_approval"} {
		said := sandboxapi.JudgePolicyAnswerAnswerYes
		if id == "supporting" || id == "claims_approval" {
			said = sandboxapi.JudgePolicyAnswerAnswerNo
		}
		answers = append(answers, sandboxapi.JudgePolicyAnswer{ID: id, Answer: said, Reason: "it is"})
	}
	harness := newPolicyJudge(t, sandboxapi.JudgePolicyReply{Answers: answers, PromptVersion: "policy-0", QuestionsDigest: "another"})
	service.SetLeases(harness)
	service.SetUses(approvedUses{use: policyUse(pullRequestPolicy("org/repo"))})

	answer, err := service.Judge(ctx, "pool-1", requestAsk())
	if err == nil && answer.Allow {
		t.Fatalf("answer = %+v, want no allow on answers to other questions", answer)
	}
	if len(harness.paths) != 1 || !strings.HasSuffix(harness.paths[0], "/judge/policy") {
		t.Fatalf("judge discobox asked at %v, want once at judge/policy", harness.paths)
	}
}
