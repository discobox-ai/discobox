package main

import (
	"slices"
	"strings"
	"testing"

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
