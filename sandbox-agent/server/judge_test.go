package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
)

// Judging is its own authority (ADR 26-09-22-838 §2): a token for a discobox's own
// work cannot ask the judge anything, whatever else it may do.
func TestJudgeRequiresItsOwnScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	job := `{"kind":"request","purpose":"open a PR","host":"api.github.com","round":1,` +
		`"request":{"method":"POST","url":"https://api.github.com/graphql"}}`

	for _, tc := range []struct {
		name, scope string
		refused     bool
	}{
		{"a token for the sandbox's own work", ScopeExecWrite, true},
		{"a token that may read what it does", ScopeExecRead, true},
		{"a token for terminals", ScopeTerminalWrite, true},
		{"a token that may ask the judge", ScopeJudgeRun, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/api/projects/project-1/sandboxes/sandbox-1/judge", strings.NewReader(job))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", tc.scope))
			router.ServeHTTP(resp, req)

			switch {
			case tc.refused && resp.Code != http.StatusForbidden:
				t.Fatalf("status = %d, want 403: this token may not ask the judge", resp.Code)
			case !tc.refused && resp.Code == http.StatusForbidden:
				t.Fatalf("status = %d, want the scope accepted", resp.Code)
			}
		})
	}
}

// A discobox that is not a judge answers no judging: this router is an
// ordinary sandbox's, and the ask reaches the service and is refused there
// rather than being answered by something that is not a judge.
func TestJudgeIsRefusedByADiscoboxThatIsNotOne(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	job := `{"kind":"command","purpose":"open a PR","host":"api.github.com","round":1,"command":["gh","pr","create"]}`

	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/projects/project-1/sandboxes/sandbox-1/judge", strings.NewReader(job))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", ScopeJudgeRun))
	router.ServeHTTP(resp, req)

	if resp.Code == http.StatusOK {
		t.Fatalf("status = %d, body = %s; want no verdict from a discobox that is not a judge", resp.Code, resp.Body.String())
	}
}

// Answering a policy's questions is the judge runtime's too, on the same scope
// and nothing else's (ADR 26-10-20-782 §8).
func TestAPolicysQuestionsRequireTheJudgeScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	job := `{"kind":"request","host":"api.github.com","round":1,` +
		`"policy":{"statements":[{"effect":"allow","actions":["read"],"resourceType":"anything","scope":{"type":"repository","name":"org/repo"}}],"definitions":{"repository":"A GitHub repository."}},` +
		`"request":{"method":"GET","url":"https://api.github.com/repos/org/repo"}}`

	for _, tc := range []struct {
		name, scope string
		refused     bool
	}{
		{"a token for the sandbox's own work", ScopeExecWrite, true},
		{"a token for terminals", ScopeTerminalWrite, true},
		{"a token that may ask the judge", ScopeJudgeRun, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/api/projects/project-1/sandboxes/sandbox-1/judge/policy", strings.NewReader(job))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", tc.scope))
			router.ServeHTTP(resp, req)

			switch {
			case tc.refused && resp.Code != http.StatusForbidden:
				t.Fatalf("status = %d, want 403: this token may not ask the judge", resp.Code)
			case !tc.refused && (resp.Code == http.StatusForbidden || resp.Code == http.StatusOK):
				t.Fatalf("status = %d, body = %s; want the scope accepted and no answers from a discobox that is not a judge", resp.Code, resp.Body.String())
			}
		})
	}
}

// Proposing a rule's wording is the judge runtime's other job, on the same
// scope and nothing else's (ADR 26-10-12-330 §7), and a discobox that is not a
// judge writes none.
func TestProposingARuleRequiresTheJudgeScope(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	job := `{"uses":["gh api GET repos/org/repo/issues/43"],"hosts":["github.com"]}`

	for _, tc := range []struct {
		name, scope string
		refused     bool
	}{
		{"a token for the sandbox's own work", ScopeExecWrite, true},
		{"a token for terminals", ScopeTerminalWrite, true},
		{"a token that may ask the judge", ScopeJudgeRun, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
				"/api/projects/project-1/sandboxes/sandbox-1/judge/proposal", strings.NewReader(job))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+signToken("project-1", "sandbox-1", "worker-1", tc.scope))
			router.ServeHTTP(resp, req)

			switch {
			case tc.refused && resp.Code != http.StatusForbidden:
				t.Fatalf("status = %d, want 403: this token may not ask the judge", resp.Code)
			case !tc.refused && (resp.Code == http.StatusForbidden || resp.Code == http.StatusOK):
				t.Fatalf("status = %d, body = %s; want the scope accepted and no proposal from a discobox that is not a judge", resp.Code, resp.Body.String())
			}
		})
	}
}

// The facts the control plane states reach the judge's wrapper as they were
// sent (ADR 26-10-14-691): a field the wire carries and the job drops is a
// judge deciding without what it was told.
func TestTheJudgeIsGivenTheFactsItWasSent(t *testing.T) {
	t.Parallel()
	facts := []string{"The discobox asking is sbx_worker.", "It was created with no source."}
	job, err := judgeJob(&sandboxapi.JudgeJob{
		Kind: sandboxapi.JudgeJobKindDelegation, Purposes: []string{"read issues"}, Host: "api.github.com", Round: 1,
		Uses: []string{"read issue 1"}, Facts: facts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(job.Facts, facts) {
		t.Fatalf("facts = %q, want %q", job.Facts, facts)
	}
}

// What a delegation was delegated reaches the judge as the list the control
// plane sent, each use whole however many lines it runs over.
func TestTheJudgeIsGivenThePurposesItWasSent(t *testing.T) {
	t.Parallel()
	purposes := []string{"Triage issues in org/repo.\nNever delete a label.", "read pulls in org/repo"}
	job, err := judgeJob(&sandboxapi.JudgeJob{
		Kind: sandboxapi.JudgeJobKindDelegation, Purposes: purposes, Host: "api.github.com", Round: 1,
		Uses: []string{"read issue 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Purpose != "" || !slices.Equal(job.Purposes, purposes) {
		t.Fatalf("job = %+v, want the purposes as sent and no single purpose", job)
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

// A policy and the values its variables bind to reach the judge as the
// control plane sent them: the questions are built from them on both sides,
// and a field dropped here is a judge answering questions nobody asked.
func TestTheJudgeIsGivenThePolicyItWasSent(t *testing.T) {
	t.Parallel()
	sent := sandboxapi.AccessPolicy{
		Statements: []sandboxapi.AccessPolicyStatement{{
			Effect: sandboxapi.AccessPolicyStatementEffectAllow, Actions: []string{"create"}, ResourceType: "issue",
			Scope: sandboxapi.AccessPolicyScope{Type: "repository", Name: sandboxapi.NewOptString("$primary.repo")},
		}},
		Definitions: sandboxapi.NewOptAccessPolicyDefinitions(sandboxapi.AccessPolicyDefinitions{
			"issue": "A GitHub issue.", "repository": "A GitHub repository.",
		}),
		Resolved: sandboxapi.NewOptAccessPolicyResolved(sandboxapi.AccessPolicyResolved{"$primary.repo": "org/repo"}),
	}
	job, err := judgeJob(&sandboxapi.JudgeJob{
		Kind: sandboxapi.JudgeJobKindRequest, Host: "api.github.com", Round: 1,
		Policy:    sandboxapi.NewOptAccessPolicy(sent),
		Variables: sandboxapi.NewOptJudgeJobVariables(sandboxapi.JudgeJobVariables{"$primary.repo": "org/repo"}),
		Request:   sandboxapi.NewOptJudgeRequestEvidence(sandboxapi.JudgeRequestEvidence{Method: "POST", URL: "https://api.github.com/repos/org/repo/issues"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Policy == nil || job.Policy.Statements[0].Scope.Name != "$primary.repo" || job.Policy.Resolved["$primary.repo"] != "org/repo" ||
		job.Variables["$primary.repo"] != "org/repo" || job.Policy.Definitions["issue"] != "A GitHub issue." {
		t.Fatalf("job = %+v, policy = %+v, want the policy and variables as sent", job, job.Policy)
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}
