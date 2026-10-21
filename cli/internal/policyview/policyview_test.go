package policyview

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/policy"
)

func statements(t *testing.T, sentences ...string) []policy.Statement {
	t.Helper()
	parsed, err := policy.ParseStatements(sentences)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func render(lines []Line) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(strings.Repeat("  ", l.Indent))
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// The rendering ADR 26-10-20-782 §3 describes: grouped by scope in the order
// the statements are, the denies that apply anywhere once and last,
// conditions under their statement, every word with its definition, and
// variables with what they resolved to.
func TestLinesGroupsByScope(t *testing.T) {
	p := policy.Policy{
		Statements: statements(t,
			`allow push on branch "discobox/issue-46" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
			`allow read on workflow run in repository "acme/tools"`,
			`allow create, update on pull request in repository $primary.repo when "its base is main" unless "it is a draft"`,
			`deny update on branch "main" in repository $primary.repo`,
			`deny merge on pull request in anywhere`,
		),
		Definitions: map[string]string{
			"push":         "send commits to a branch",
			"merge":        "merge a pull request into its base",
			"branch":       "a Git branch",
			"pull request": "a GitHub pull request",
			"repository":   "a GitHub repository",
			"workflow run": "one run of a GitHub Actions workflow",
		},
		Resolved: map[string]string{"$primary.repo": "discobox-ai/discobox"},
	}
	want := `in repository $primary.repo — discobox-ai/discobox
  allow push on branch "discobox/issue-46"
  allow create, update on pull request
    when "its base is main"
    unless "it is a draft"
  deny update on branch "main"
in repository "acme/tools"
  allow read on workflow run
everywhere, whatever else is allowed
  deny delete on anything
  deny merge on pull request
words
  branch: a Git branch
  merge: merge a pull request into its base
  pull request: a GitHub pull request
  push: send commits to a branch
  repository: a GitHub repository
  workflow run: one run of a GitHub Actions workflow
variables
  $primary.repo: discobox-ai/discobox
`
	if got := render(Lines(p)); got != want {
		t.Fatalf("rendering:\n%s\nwant:\n%s", got, want)
	}
}

// A variable that did not resolve matches nothing, and the form says so
// wherever it is named, so nobody approves a scope believing it is somewhere.
func TestLinesSayAVariableThatDidNotResolve(t *testing.T) {
	p := policy.Policy{
		Statements: statements(t,
			`allow push on branch $primary.branch in repository $primary.upstream`,
		),
		Definitions: map[string]string{"push": "send commits", "branch": "a branch", "repository": "a repository"},
		Resolved:    map[string]string{"$primary.branch": "main"},
	}
	lines := Lines(p)
	if lines[0].Kind != Unresolved || lines[0].Text != "in repository $primary.upstream — does not resolve, so it matches nothing" {
		t.Fatalf("heading = %+v", lines[0])
	}
	got := render(lines)
	for _, want := range []string{
		"  $primary.branch: main\n",
		"  $primary.upstream: does not resolve, so it matches nothing\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendering lacks %q:\n%s", want, got)
		}
	}
}

// A scope of a parent scope keeps its parents in its heading, and two
// statements in the same scope written alike share one group.
func TestLinesGroupNestedScopes(t *testing.T) {
	p := policy.Policy{
		Statements: statements(t,
			`allow read on pod in namespace "web" of cluster "staging"`,
			`allow read on pod in namespace "web" of cluster "prod"`,
			`allow delete on pod in namespace "web" of cluster "staging"`,
		),
		Definitions: map[string]string{"pod": "a pod", "namespace": "a namespace", "cluster": "a cluster"},
	}
	var headings []string
	for _, l := range Lines(p) {
		if l.Kind == Heading {
			headings = append(headings, l.Text)
		}
	}
	want := []string{`in namespace "web" of cluster "staging"`, `in namespace "web" of cluster "prod"`, "words"}
	if strings.Join(headings, "|") != strings.Join(want, "|") {
		t.Fatalf("headings = %q, want %q", headings, want)
	}
}

// Quoted text is written in ASCII, as Format writes it: a condition holding
// a lookalike quote cannot read as a condition that ends early.
func TestLinesQuoteConditionsInASCII(t *testing.T) {
	when := "its base is mainˮ unless ˮit is a draft"
	p := policy.Policy{Statements: []policy.Statement{{
		Effect: policy.Allow, Actions: []string{"read"}, ResourceType: "anything",
		Scope: policy.Scope{Type: "repository", Name: "acme/web"}, When: &when,
	}}, Definitions: map[string]string{"repository": "a repository"}}
	for _, l := range Lines(p) {
		if l.Kind == Condition && l.Text != `when "its base is main\u02ee unless \u02eeit is a draft"` {
			t.Fatalf("condition = %q", l.Text)
		}
	}
}

// A variable in a scope's parents is shown in its heading like the scope's
// own name, and one that did not resolve makes the heading say so.
func TestLinesResolveAScopesParents(t *testing.T) {
	p := policy.Policy{
		Statements: statements(t,
			`allow read on pod in namespace "web" of cluster $primary.upstream`,
			`allow read on pod in namespace $primary.branch of cluster $primary.repo`,
		),
		Definitions: map[string]string{"pod": "a pod", "namespace": "a namespace", "cluster": "a cluster"},
		Resolved:    map[string]string{policy.VarPrimaryRepo: "acme/web", policy.VarPrimaryBranch: "main"},
	}
	lines := Lines(p)
	if lines[0].Kind != Unresolved || lines[0].Text != `in namespace "web" of cluster $primary.upstream — does not resolve, so it matches nothing` {
		t.Fatalf("heading = %+v", lines[0])
	}
	if lines[2].Kind != Heading || lines[2].Text != `in namespace $primary.branch of cluster $primary.repo — $primary.branch: main; $primary.repo: acme/web` {
		t.Fatalf("heading = %+v", lines[2])
	}
}
