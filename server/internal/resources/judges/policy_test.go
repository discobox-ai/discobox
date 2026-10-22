package judges

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// pullRequestPolicy is a grant's policy for sandbox-1, created from
// github.com/org/repo, approved with $primary.repo resolved to repo.
func pullRequestPolicy(repo string) *policy.Policy {
	name := "discobox/issue-46"
	return &policy.Policy{
		Statements: []policy.Statement{
			{Effect: policy.Allow, Actions: []string{policy.ActionCreate, policy.ActionUpdate}, ResourceType: "pull request",
				Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo}},
			{Effect: policy.Allow, Actions: []string{"push"}, ResourceType: "branch", Name: &name,
				Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo}},
			{Effect: policy.Deny, Actions: []string{"force-push"}, ResourceType: "branch", Scope: policy.Scope{Type: policy.Anywhere}},
		},
		Definitions: map[string]string{
			"pull request": "A GitHub pull request.",
			"repository":   "A GitHub repository, owner/name.",
			"push":         "Update a Git ref.",
			"force-push":   "Push a ref so that commits it pointed at are no longer on it.",
			"branch":       "A Git branch.",
		},
		Resolved: map[string]string{policy.VarPrimaryRepo: repo},
	}
}

var pullRequestStatements = []string{
	`allow create, update on pull request in repository "org/repo"`,
	`allow push on branch "discobox/issue-46" in repository "org/repo"`,
	`deny force-push on branch in anywhere`,
}

func policyUse(p *policy.Policy) services.ApprovedUse {
	return services.ApprovedUse{
		Purpose: "open a pull request in org/repo", Policy: p,
		Host: "api.github.com", Credential: "GitHub token", GrantID: "grant-1",
	}
}

// A request and a command under a grant that carries a policy are judged
// against the policy's statements, bound to the discobox asking, with their
// definitions, and not against the sentence of the use they named
// (ADR 26-10-20-782 §1). What is recorded is the job as the judge read it.
func TestAJobUnderAPolicyIsJudgedAgainstItsStatements(t *testing.T) {
	for _, kind := range []string{judge.KindRequest, judge.KindCommand} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			service, appStore, fake := standingJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "that opens the pull request"})
			service.SetUses(approvedUses{use: policyUse(pullRequestPolicy("org/repo"))})

			var err error
			if kind == judge.KindCommand {
				_, err = service.JudgeCommand(ctx, "pool-1", commandAsk())
			} else {
				_, err = service.Judge(ctx, "pool-1", requestAsk())
			}
			if err != nil {
				t.Fatalf("judging error = %v", err)
			}
			jobs := fake.asked()
			if len(jobs) != 1 {
				t.Fatalf("the judge was asked %d times, want once", len(jobs))
			}
			job := jobs[0]
			definitions, _ := job.Definitions.Get()
			if job.Purpose.IsSet() || !slices.Equal(job.Purposes, pullRequestStatements) ||
				!maps.Equal(map[string]string(definitions), pullRequestPolicy("org/repo").Definitions) {
				t.Fatalf("job = %+v, want the bound statements and their definitions in place of the sentence", job)
			}
			if !slices.Equal(job.Facts, workerFacts) {
				t.Fatalf("facts = %q, want the discobox asking's", job.Facts)
			}

			verdicts, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{})
			if err != nil || len(verdicts) != 1 {
				t.Fatalf("verdicts = %d, %v; want one", len(verdicts), err)
			}
			var recorded judge.Job
			if err := json.Unmarshal([]byte(verdicts[0].Prompt), &recorded); err != nil ||
				recorded.Purpose != "" || !slices.Equal(recorded.Purposes, pullRequestStatements) || len(recorded.Definitions) != 5 {
				t.Fatalf("prompt = %s, want the policy job as the judge read it", verdicts[0].Prompt)
			}
		})
	}
}

// A policy whose allows bind to nothing for the discobox asking — approved
// against another repository than the one it was created from — allows
// nothing, and is refused without asking a judge.
func TestAPolicyThatAllowsNothingHereIsRefusedUnasked(t *testing.T) {
	for _, kind := range []string{judge.KindRequest, judge.KindCommand} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			service, _, fake := standingJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "fine"})
			service.SetUses(approvedUses{use: policyUse(pullRequestPolicy("org/other"))})

			var err error
			if kind == judge.KindCommand {
				_, err = service.JudgeCommand(ctx, "pool-1", commandAsk())
			} else {
				_, err = service.Judge(ctx, "pool-1", requestAsk())
			}
			var status apperrors.StatusError
			if !errors.As(err, &status) || status.Status != http.StatusForbidden {
				t.Fatalf("judging error = %v, want a refusal", err)
			}
			if n := len(fake.asked()); n != 0 {
				t.Fatalf("the judge was asked %d times, want none", n)
			}
		})
	}
}

// A standing allow was the judge's reading of what it was shown: a use now
// judged against a policy, or against a policy edited since, has been read
// by nobody, and is asked about again.
func TestAStandingAllowEndsWithThePolicyItWasGrantedUnder(t *testing.T) {
	route := "GET /repos/org/repo/pulls"
	url := "https://api.github.com/repos/org/repo/pulls"
	edited := pullRequestPolicy("org/repo")
	edited.Statements = edited.Statements[:2]
	for _, tc := range []struct {
		name  string
		first services.ApprovedUse
		then  services.ApprovedUse
		asked int
	}{
		{"the same policy", policyUse(pullRequestPolicy("org/repo")), policyUse(pullRequestPolicy("org/repo")), 1},
		{"a sentence, then a policy", policyUse(nil), policyUse(pullRequestPolicy("org/repo")), 2},
		{"a policy, then edited", policyUse(pullRequestPolicy("org/repo")), policyUse(edited), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, _, fake := standingJudge(t, standingAnswer(route, 600))
			service.SetUses(approvedUses{use: tc.first})
			if _, err := service.Judge(ctx, "pool-1", getAsk("sandbox-1", url)); err != nil {
				t.Fatal(err)
			}
			service.SetUses(approvedUses{use: tc.then})
			if _, err := service.Judge(ctx, "pool-1", getAsk("sandbox-1", url)); err != nil {
				t.Fatal(err)
			}
			if n := len(fake.asked()); n != tc.asked {
				t.Fatalf("the judge was asked %d times, want %d", n, tc.asked)
			}
		})
	}
}
