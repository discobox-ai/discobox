package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/judge"
)

// Every recorded case loads as the runner asks it, its policy included: each
// policy parses, validates, binds to its discobox and fits a judge job. A
// broken case fails here rather than as a refusal in a run nobody reads
// closely.
func TestTheRecordedCasesLoad(t *testing.T) {
	cases, err := loadCases("../../../test/judge-evals/cases", "*")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Case{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	policies := 0
	for _, c := range cases {
		if c.sentence == "" {
			continue
		}
		policies++
		if _, ok := byName[c.sentence]; !ok {
			t.Errorf("%s has no sentence case beside it", c.Name)
		}
	}
	if policies == 0 {
		t.Fatal("no case is asked under a policy")
	}

	// A policy case is told what the server states from its record: who is
	// asking, and the discobox a call names.
	created := byName["facts-target-a-discobox-it-created"+policySuffix]
	if !slices.Equal(created.Job.Facts, judge.TargetFacts(judge.DiscoboxOf(*byName["facts-target-a-discobox-it-created"].Policy.Facts),
		judge.DiscoboxOf(*byName["facts-target-a-discobox-it-created"].Policy.Target))) {
		t.Fatalf("facts = %q, want the server's", created.Job.Facts)
	}
	if !slices.ContainsFunc(created.Job.Purposes, func(s string) bool { return strings.Contains(s, `created by "sbx_4kq8w2m7v9t3x6pn"`) }) {
		t.Fatalf("statements = %q, want $this bound to the discobox asking", created.Job.Purposes)
	}

	// A policy whose allow binds to nothing is refused without asking.
	if unstated := byName["facts-create-an-issue-in-this-discoboxs-repository-unstated"+policySuffix]; !unstated.allowsNothing {
		t.Fatalf("%s is asked, want it refused unasked: $primary.repo has no value", unstated.Name)
	}
}

// A glob selects a policy case by its own name, without its sentence case.
func TestAGlobSelectsThePolicyCaseAlone(t *testing.T) {
	cases, err := loadCases("../../../test/judge-evals/cases", "dev-git-fetch-a-repo"+policySuffix)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].Name != "dev-git-fetch-a-repo"+policySuffix {
		t.Fatalf("cases = %v, want the policy case alone", cases)
	}
}

// Every rule case is held to its rule by code at load, as the server holds
// a request: a case code decides scores as it expects without a judge, and
// one it leaves open puts only well-formed rule jobs to one. Both kinds are
// recorded, so the evals measure the judge and code alike.
func TestTheRuleCasesCodeDecidesScoreAsExpected(t *testing.T) {
	cases, err := loadCases("../../../test/judge-evals/cases", "*")
	if err != nil {
		t.Fatal(err)
	}
	var code, judged []string
	for _, c := range cases {
		if c.Rule == nil {
			continue
		}
		if len(c.held.Questions) == 0 {
			code = append(code, c.Name)
			if want := c.Expect == ExpectAllow; c.held.Covered() != want {
				t.Errorf("%s: code found covered %v (%s), want %s", c.Name, c.held.Covered(), c.held.Reason, c.Expect)
			}
			continue
		}
		judged = append(judged, c.Name)
		for _, jobs := range c.held.Questions {
			for _, job := range jobs {
				if err := job.Validate(); err != nil {
					t.Errorf("%s: %v", c.Name, err)
				}
				// Bound for the discobox that asked: a judge reads values,
				// never a variable.
				if strings.Contains(job.Purposes[0]+job.Uses[0], "$") {
					t.Errorf("%s: %q against %q is not bound", c.Name, job.Uses[0], job.Purposes[0])
				}
			}
		}
	}
	if len(code) == 0 || len(judged) == 0 {
		t.Fatalf("decided by code %q, asked of a judge %q: want rule cases of both", code, judged)
	}
	t.Logf("decided by code: %d %q", len(code), code)
	t.Logf("asked of a judge: %d %q", len(judged), judged)
}

// A rule case asks the judge about conditions alone, one rule allow against
// one allow asked for, and is covered only when it allows.
func TestARuleCaseAsksTheJudgeOnlyItsConditions(t *testing.T) {
	cases, err := loadCases("../../../test/judge-evals/cases", "rule-develop-any-slug")
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{true, false} {
		var asked []judge.Job
		judging := func(_ context.Context, job judge.Job, _ time.Duration, _ string) verdict {
			asked = append(asked, job)
			if allow {
				return verdict{outcome: OutcomeAllow}
			}
			return verdict{outcome: OutcomeRefuse}
		}
		run := judgeRule(context.Background(), judging, cases[0], time.Minute, "")
		want := OutcomeRefuse
		if allow {
			want = OutcomeAllow
		}
		if run.Outcome != want || run.Code {
			t.Fatalf("judge allows %v: run = %+v, want %s from the judge", allow, run, want)
		}
		if len(asked) != 1 ||
			asked[0].Purposes[0] != `allow create, update on pull request in repository "discobox-ai/discobox" when "its head is discobox/* and its base is main"` ||
			asked[0].Uses[0] != `allow create, update on pull request in repository "discobox-ai/discobox" when "its head is discobox/26-10-12-330 and its base is main"` {
			t.Fatalf("asked %+v, want the pull request's allows alone, bound", asked)
		}
	}
}

// A proposal is held as a rule is: the policy written covers the request,
// and code refuses each control, so a judge that allows everything asked
// passes a proposal that keeps its denies and fails one that drops one.
func TestAProposalIsHeldAsARuleIs(t *testing.T) {
	cases, err := loadCases("../../../test/judge-evals/cases", "propose-develop")
	if err != nil {
		t.Fatal(err)
	}
	c := cases[0]
	anyTarget := make([]string, 0, len(c.Proposal.Request.Statements))
	for _, s := range c.Proposal.Request.Statements {
		anyTarget = append(anyTarget, strings.ReplaceAll(s, "discobox/26-10-12-330", "discobox/*"))
	}
	allowAll := func(context.Context, judge.Job, time.Duration, string) verdict {
		return verdict{outcome: OutcomeAllow}
	}
	wrapper := []string{"sh", "-c", `printf '%s' "$DISCOBOX_TEST_PROPOSAL"`, "sh"}
	for _, tc := range []struct {
		name       string
		statements []string
		pass       bool
	}{
		{"any target", anyTarget, true},
		{"a deny dropped", slices.DeleteFunc(slices.Clone(anyTarget), func(s string) bool { return strings.Contains(s, "merge") }), false},
		{"a word nobody defined", append(slices.Clone(anyTarget), `deny publish on package in anywhere`), false},
	} {
		data, err := json.Marshal(judge.Proposal{StatementsAnyTarget: tc.statements})
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DISCOBOX_TEST_PROPOSAL", string(data))
		run := propose(context.Background(), wrapper, judge.Role, allowAll, c, time.Minute, "")
		if run.Pass != tc.pass {
			t.Errorf("%s: run = %+v, want pass %v", tc.name, run, tc.pass)
		}
	}
}
