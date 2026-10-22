package policyview

import (
	"slices"
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
// the statements are, the denies that apply anywhere once and last under
// "global", conditions under their statement, each variable followed by its
// value, and each word that is not a default followed by its definition
// where it is first used.
func TestLinesGroupsByScope(t *testing.T) {
	p := policy.Policy{
		Statements: statements(t,
			`allow push on branch "discobox/issue-46" in repository $primary.repo`,
			`deny delete on anything in anywhere`,
			`allow read on workflow run in repository "acme/tools"`,
			`allow create, update on pull request created by $this in repository $primary.repo when "its base is main" unless "it is a draft"`,
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
		Resolved: map[string]string{"$primary.repo": "discobox-ai/discobox", "$this": "sbx_1"},
	}
	want := `in repository (a GitHub repository) $primary.repo (discobox-ai/discobox)
  allow push (send commits to a branch) on branch (a Git branch) "discobox/issue-46"
  allow create, update on pull request (a GitHub pull request) created by $this (sbx_1)
    when "its base is main"
    unless "it is a draft"
  deny update on branch "main"
in repository "acme/tools"
  allow read on workflow run (one run of a GitHub Actions workflow)
global
  deny delete on anything
  deny merge (merge a pull request into its base) on pull request
`
	if got := render(Lines(p)); got != want {
		t.Fatalf("rendering:\n%s\nwant:\n%s", got, want)
	}
}

// A line's tokens are the sentence's, kind by kind, each noted where the text
// is: a surface highlights them without reading the text.
func TestLinesCarryTheSentencesTokens(t *testing.T) {
	p := policy.Policy{
		Statements:  statements(t, `allow push on branch $primary.branch in repository "acme/web"`),
		Definitions: map[string]string{"push": "send commits", "branch": "a branch", "repository": "a repository"},
		Resolved:    map[string]string{"$primary.branch": "main"},
	}
	statement := Lines(p)[1]
	var kinds []policy.TokenKind
	var notes []string
	for _, tok := range statement.Tokens {
		kinds = append(kinds, tok.Kind)
		notes = append(notes, tok.Note)
	}
	wantKinds := []policy.TokenKind{policy.TokenEffect, policy.TokenAction, policy.TokenKeyword, policy.TokenType, policy.TokenVariable}
	wantNotes := []string{"", "send commits", "", "a branch", "main"}
	if !slices.Equal(kinds, wantKinds) || !slices.Equal(notes, wantNotes) {
		t.Fatalf("tokens = %+v", statement.Tokens)
	}
}

// A variable that did not resolve matches nothing, and the form says so
// wherever it is named, so nobody approves a scope believing it is somewhere.
func TestLinesSayAVariableThatDidNotResolve(t *testing.T) {
	p := policy.Policy{
		Statements:  statements(t, `allow push on branch $primary.branch in repository $primary.upstream`),
		Definitions: map[string]string{"push": "send commits", "branch": "a branch", "repository": "a repository"},
		Resolved:    map[string]string{"$primary.branch": "main"},
	}
	lines := Lines(p)
	if lines[0].Kind != Unresolved || lines[0].Text != "in repository (a repository) $primary.upstream (does not resolve, so it matches nothing)" {
		t.Fatalf("heading = %+v", lines[0])
	}
	if lines[1].Text != "allow push (send commits) on branch (a branch) $primary.branch (main)" {
		t.Fatalf("statement = %q", lines[1].Text)
	}
}

// A rule's policy binds its variables to whichever discobox asks, so it is
// drawn with each variable bare: none is said not to resolve, and no value
// recorded beside it is shown as the one a later request is held to.
func TestUnboundDrawsVariablesBare(t *testing.T) {
	p := policy.Policy{
		Statements:  statements(t, `allow push on branch $primary.branch in repository $primary.upstream`),
		Definitions: map[string]string{"push": "send commits", "branch": "a branch", "repository": "a repository"},
		Resolved:    map[string]string{"$primary.branch": "main"},
	}
	lines := Unbound(p)
	if lines[0].Kind != Heading || lines[0].Text != "in repository (a repository) $primary.upstream" {
		t.Fatalf("heading = %+v", lines[0])
	}
	if lines[1].Text != "allow push (send commits) on branch (a branch) $primary.branch" {
		t.Fatalf("statement = %q", lines[1].Text)
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
	}
	var headings []string
	for _, l := range Lines(p) {
		if l.Kind == Heading {
			headings = append(headings, l.Text)
		}
	}
	want := []string{`in namespace "web" of cluster "staging"`, `in namespace "web" of cluster "prod"`}
	if strings.Join(headings, "|") != strings.Join(want, "|") {
		t.Fatalf("headings = %q, want %q", headings, want)
	}
}

// A variable in a scope's parents is noted like the scope's own name, and
// one that did not resolve makes the heading Unresolved.
func TestLinesResolveAScopesParents(t *testing.T) {
	p := policy.Policy{
		Statements: statements(t,
			`allow read on pod in namespace "web" of cluster $primary.upstream`,
			`allow read on pod in namespace $primary.branch of cluster $primary.repo`,
		),
		Resolved: map[string]string{policy.VarPrimaryRepo: "acme/web", policy.VarPrimaryBranch: "main"},
	}
	lines := Lines(p)
	if lines[0].Kind != Unresolved || lines[0].Text != `in namespace "web" of cluster $primary.upstream (does not resolve, so it matches nothing)` {
		t.Fatalf("heading = %+v", lines[0])
	}
	if lines[2].Kind != Heading || lines[2].Text != `in namespace $primary.branch (main) of cluster $primary.repo (acme/web)` {
		t.Fatalf("heading = %+v", lines[2])
	}
}

// Quoted text is written in ASCII, as Format writes it: a condition holding
// a lookalike quote cannot read as a condition that ends early.
func TestLinesQuoteConditionsInASCII(t *testing.T) {
	when := "its base is mainˮ unless ˮit is a draft"
	p := policy.Policy{Statements: []policy.Statement{{
		Effect: policy.Allow, Actions: []string{"read"}, ResourceType: "anything",
		Scope: policy.Scope{Type: "repository", Name: "acme/web"}, When: &when,
	}}}
	for _, l := range Lines(p) {
		if l.Kind == Condition && l.Text != `when "its base is main\u02ee unless \u02eeit is a draft"` {
			t.Fatalf("condition = %q", l.Text)
		}
	}
}
