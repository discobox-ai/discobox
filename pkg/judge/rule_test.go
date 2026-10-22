package judge_test

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/pkg/policy"
)

func rulePolicy(t *testing.T, sentences ...string) policy.Policy {
	t.Helper()
	statements, err := policy.ParseStatements(sentences)
	if err != nil {
		t.Fatal(err)
	}
	return policy.Policy{Statements: statements, Definitions: map[string]string{"pull request": "A GitHub pull request.", "repository": "A GitHub repository."}}
}

var asking = policy.Facts{Discobox: policy.Discobox{ID: "sbx_1"}, Sources: []policy.Source{{Slug: "primary", Primary: true, Remote: &policy.Remote{Host: "github.com", Repo: "org/repo", URL: "https://github.com/org/repo"}}}}

// Code decides what structure settles; a condition it cannot compare is one
// rule job, the two allows bound for the discobox that asked.
func TestHoldToRule(t *testing.T) {
	rule := rulePolicy(t, `allow create, update on pull request in repository $primary.repo when "its head is discobox/*"`)
	exact, err := judge.HoldToRule(rule, rule, asking, "com.github.api", []string{"github.com"})
	if err != nil || !exact.Covered() || exact.Values[policy.VarPrimaryRepo] != "org/repo" {
		t.Fatalf("the rule's own policy = %+v, %v, want covered by code", exact, err)
	}
	wider, err := judge.HoldToRule(rulePolicy(t, `allow manage on pull request in repository $primary.repo`), rule, asking, "", []string{"github.com"})
	if err != nil || wider.Covered() || wider.Reason == "" {
		t.Fatalf("a wider ask = %+v, %v, want refused by code", wider, err)
	}
	asked := rulePolicy(t, `allow update on pull request in repository $primary.repo when "its head is discobox/issue-4"`)
	held, err := judge.HoldToRule(asked, rule, asking, "com.github.api", []string{"github.com"})
	if err != nil || held.Covered() || held.Reason != "" || len(held.Questions) != 1 || len(held.Questions[0]) != 1 {
		t.Fatalf("a narrower condition = %+v, %v, want one question", held, err)
	}
	if job := held.Questions[0][0]; job.Kind != judge.KindRule || !strings.Contains(job.Uses[0], `"org/repo"`) || !strings.Contains(job.Purposes[0], "discobox/*") {
		t.Fatalf("job = %+v, want the two allows, bound", job)
	}
	nothing, err := judge.HoldToRule(rulePolicy(t, `allow update on pull request in repository $primary.upstream`), rule, asking, "", []string{"github.com"})
	if err != nil || nothing.Covered() || !strings.Contains(nothing.Reason, "allows nothing") {
		t.Fatalf("an ask that resolves to nothing = %+v, %v, want refused", nothing, err)
	}
}

// What a model wrote takes the source's definitions for its words, and is
// refused when it brings in one, or is no policy a rule may have.
func TestWrittenPolicy(t *testing.T) {
	definitions := map[string]string{"pull request": "A GitHub pull request.", "repository": "A GitHub repository.", "unused": "x"}
	written, err := judge.WrittenPolicy([]string{`allow update on pull request in repository $primary.repo`}, definitions)
	if err != nil || len(written.Definitions) != 2 {
		t.Fatalf("WrittenPolicy() = %+v, %v, want the words it uses defined", written, err)
	}
	for name, sentences := range map[string][]string{
		"a word nobody defined": {`allow push on branch in repository $primary.repo`},
		"denies alone":          {`deny delete on anything in anywhere`},
		"no sentence":           {"update pull requests"},
	} {
		if _, err := judge.WrittenPolicy(sentences, definitions); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
