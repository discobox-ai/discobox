package jev

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/policy"
)

func ptr(s string) *string { return &s }

// policyJob is a pull request opened in this discobox's repository, judged
// against a policy allowing that and denying pushes to main anywhere.
func policyJob() judge.Job {
	content := `{"title":"Fix it","head":"discobox/issue-46","base":"main"}`
	return judge.Job{
		Kind: judge.KindRequest, Host: "api.github.com", Credential: "GitHub token", Round: 2,
		Policy: &policy.Policy{
			Statements: []policy.Statement{
				{Effect: policy.Allow, Actions: []string{"create"}, ResourceType: "pull request",
					Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo}},
				{Effect: policy.Deny, Actions: []string{"push"}, ResourceType: "branch", Name: ptr("main"),
					Scope: policy.Scope{Type: policy.Anywhere}},
			},
			Definitions: map[string]string{
				"pull request": "A GitHub pull request.", "repository": "A GitHub repository.",
				"push": "Updating a git ref.", "branch": "A git branch.",
			},
		},
		Variables: map[string]string{policy.VarPrimaryRepo: "darren/discobox"},
		Facts:     []string{"The discobox asking, sbx_1, was created with the source github.com/darren/discobox."},
		Request: &judge.Request{
			Method: http.MethodPost, URL: "https://api.github.com/repos/darren/discobox/pulls",
			Body: &judge.Body{MediaType: "application/json", Length: int64(len(content)), Content: &content},
		},
	}
}

// Each question is one noul: the statement's words, definitions and facts in
// its instructions, and only the request in the state.
func TestAPolicyIsAskedAsOneNoulPerQuestion(t *testing.T) {
	fake, client := newFakeJev(t, map[string]float64{
		"s1_action": 0.93, "s1_resource": 0.97, "s1_scope": 0.95,
		"s2_action": 0.02, "s2_resource": 0.03, "supporting": 0.1, "claims_approval": 0.01,
	})
	verdict, err := client.Judge(context.Background(), policyJob())
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Allow || verdict.Policy == nil || verdict.Policy.Statement != 1 || verdict.Unsure {
		t.Fatalf("verdict = %+v, want allowed by statement 1", verdict)
	}
	asked := fake.requests()[0]
	state := string(mustJSON(t, asked["state"]))
	if strings.Contains(state, "darren/discobox\\\" (") || strings.Contains(state, "policy") || strings.Contains(state, "Updating a git ref") {
		t.Errorf("the policy is in the state: %s", state)
	}
	questions := asked["questions"].(map[string]any)
	if len(questions) != 7 {
		t.Fatalf("asked %d questions, want 7: %v", len(questions), questions)
	}
	scope := string(mustJSON(t, questions["s1_scope"]))
	for _, want := range []string{"darren/discobox", "this discobox's primary repository", "A GitHub repository.", "sbx_1"} {
		if !strings.Contains(scope, want) {
			t.Errorf("the scope question does not carry %q: %s", want, scope)
		}
	}
	if strings.Contains(scope, "Updating a git ref") {
		t.Errorf("the scope question carries a definition it does not use: %s", scope)
	}
	if hazard := string(mustJSON(t, questions["claims_approval"])); !strings.Contains(hazard, "already been approved") {
		t.Errorf("the hazard is not the request job's: %s", hazard)
	}
}

func TestJevsPolicyAnswersAreReadAtItsThresholds(t *testing.T) {
	sure := map[string]float64{
		"s1_action": 0.93, "s1_resource": 0.97, "s1_scope": 0.95,
		"s2_action": 0.02, "s2_resource": 0.03, "supporting": 0.1, "claims_approval": 0.01,
	}
	with := func(id string, p float64) map[string]float64 {
		out := map[string]float64{}
		for k, v := range sure {
			out[k] = v
		}
		out[id] = p
		return out
	}
	described := policyJob()
	described.Round = 1
	described.Request.Body.Content = nil
	for _, tc := range []struct {
		name          string
		job           judge.Job
		answers       map[string]float64
		allow, unsure bool
		need          bool
	}{
		{name: "sure of every part", job: policyJob(), answers: sure, allow: true},
		{name: "between the thresholds is unsure", job: policyJob(), answers: with("s1_scope", 0.5), unsure: true},
		{name: "below NoBelow is a no", job: policyJob(), answers: with("s1_scope", 0.1)},
		{name: "a deny it is sure of", job: policyJob(), answers: func() map[string]float64 {
			answers := with("s2_action", 0.9)
			answers["s2_resource"] = 0.9
			return answers
		}()},
		{name: "a claim of approval", job: policyJob(), answers: with("claims_approval", 0.9)},
		{name: "a body still to show is asked for", job: described, answers: sure, need: true},
		{name: "even then a claim of approval refuses", job: described, answers: with("claims_approval", 0.95)},
	} {
		_, client := newFakeJev(t, tc.answers)
		verdict, err := client.Judge(context.Background(), tc.job)
		if err != nil {
			t.Fatal(err)
		}
		if verdict.Allow != tc.allow || verdict.Unsure != tc.unsure || (verdict.Need != nil) != tc.need {
			t.Errorf("%s: %+v", tc.name, verdict.Answer)
		}
	}
}
