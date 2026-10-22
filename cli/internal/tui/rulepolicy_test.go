package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/policyview"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// mustPolicy is the policy lines a rule is edited in say, for a fixture.
func mustPolicy(lines ...string) *policy.Policy {
	p, problems := parsedPolicy(lines)
	if len(problems) > 0 {
		panic(strings.Join(problems, "; "))
	}
	return p
}

// askedPolicy is the policy policyRequest asks for, as the server sends it:
// resolved for the discobox that asked.
func askedPolicy() *policy.Policy {
	p := mustPolicy(`allow create on pull request in repository $primary.repo`,
		"pull request = a pull request on GitHub", "repository = a GitHub repository")
	p.Resolved = map[string]string{"$primary.repo": "acme/pool"}
	return p
}

// policyRequest is waitingRequest asking for a policy beside its uses: the
// only kind of request a rule answers.
func policyRequest() CredentialRequest {
	req := waitingRequest()
	req.Policy, req.PolicyShown = askedPolicy(), askedPolicy()
	req.PolicyLines = policyview.Lines(*req.Policy)
	return req
}

// A rule's policy is edited as the lines discobox-access writes one in: each
// statement its sentence, then each definition as word = what it means, and
// read back into the same policy.
func TestAPolicyIsEditedAsItsSentencesAndDefinitions(t *testing.T) {
	t.Parallel()
	p := askedPolicy()
	lines := policyText(p)
	want := []string{
		`allow create on pull request in repository $primary.repo`,
		"pull request = a pull request on GitHub",
		"repository = a GitHub repository",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	read, problems := parsedPolicy(lines)
	if len(problems) > 0 || !samePolicy(read, p) {
		t.Fatalf("read back = %+v, %q; want the same policy", read, problems)
	}
	// What a variable resolved to is no part of what a rule says.
	if read.Resolved != nil {
		t.Fatalf("resolved = %v, want none on a policy read from its lines", read.Resolved)
	}
}

// What keeps lines from being a rule's policy is said a line at a time: a
// sentence that does not parse, a word with no definition, a word defined
// twice, and a policy of denies alone, which approves nothing.
func TestWhatIsNotAPolicyIsSaid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
		want  string
	}{
		{"a sentence that does not parse", []string{"allow read issue in repository $primary.repo"}, `line 1: at "in"`},
		{"a word with no definition", []string{"allow read on issue in repository $primary.repo", "repository = a GitHub repository"}, `"issue" is not a default word and has no definition`},
		{"a word defined twice", []string{"allow read on issue in repository $primary.repo", "issue = one", "issue = two", "repository = a repo"}, `line 3: "issue" is defined twice`},
		{"denies alone", []string{"deny delete on anything in anywhere"}, "these are all denies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, problems := parsedPolicy(tc.lines)
			if p != nil || !strings.Contains(strings.Join(problems, "\n"), tc.want) {
				t.Fatalf("policy = %+v, problems = %q; want none, and %q", p, problems, tc.want)
			}
		})
	}
	// A statement is a statement whatever its condition holds after an "=".
	p, problems := parsedPolicy([]string{`allow read on issue in repository $primary.repo when "its label = bug"`, "issue = a GitHub issue", "repository = a GitHub repository"})
	if len(problems) > 0 || len(p.Statements) != 1 || *p.Statements[0].When != "its label = bug" {
		t.Fatalf("policy = %+v, problems = %q; want one statement with its condition", p, problems)
	}
}
