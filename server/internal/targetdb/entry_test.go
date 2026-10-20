package targetdb

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/policy"
)

const digest = "3177ad6a86ca04c57a9bef3fc24e28a339efb8eb040c6f950af3d3ed2fee7fa3"

func github() Entry {
	return Entry{
		Host: "github.com",
		Documents: []Document{
			{Name: "rest", Kind: OpenAPI, URL: "https://raw.githubusercontent.com/github/rest-api-description/755ee33/api.github.com.json", SHA256: digest},
			{Name: "git", Kind: Prose, SHA256: strings.Repeat("a", 64)},
		},
		Terms: []Term{
			{Word: "issue", Kind: TermType, Definition: "A GitHub issue."},
			{Word: "comment", Kind: TermType, Definition: "A comment on an issue or pull request."},
			{Word: "repository", Kind: TermType, Definition: "A GitHub repository, owner/name."},
			{Word: "push", Kind: TermAction, Definition: "Update refs with git-receive-pack."},
			{Word: "branch", Kind: TermType, Definition: "A git branch."},
		},
		Statements: []Shape{
			{Action: "create", ResourceType: "issue", ScopeType: "repository", Operations: []string{"POST /repos/{owner}/{repo}/issues"}},
			{Action: "read", ResourceType: "issue", ScopeType: "repository", Operations: []string{"GET /repos/{owner}/{repo}/issues/{issue_number}"}},
			{Action: "create", ResourceType: "comment", Within: []string{"issue"}, ScopeType: "repository", Operations: []string{"POST /repos/{owner}/{repo}/issues/{issue_number}/comments"}},
			{Action: "push", ResourceType: "branch", ScopeType: "repository", Operations: []string{"git-receive-pack"}},
		},
	}
}

func TestValidateAcceptsAnEntry(t *testing.T) {
	if err := github().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Entry)
		want string
	}{
		"an empty host, which would cover every host": {func(e *Entry) { e.Host = "" }, "host is empty"},
		"a host with a port":                          {func(e *Entry) { e.Host = "github.com:443" }, "without a port"},
		"a host that is no DNS name":                  {func(e *Entry) { e.Host = "git_hub.com" }, "not a DNS name"},
		"a document named twice":                      {func(e *Entry) { e.Documents[1].Name = "rest" }, `"rest" is named twice`},
		"a document name that is no path segment":     {func(e *Entry) { e.Documents[0].Name = "../rest" }, "is not lowercase letters"},
		"an unknown kind":                             {func(e *Entry) { e.Documents[0].Kind = "wsdl" }, `kind "wsdl"`},
		"a digest that is not hex":                    {func(e *Entry) { e.Documents[0].SHA256 = "abc" }, "not a lowercase hex SHA-256"},
		"a URL that is not https":                     {func(e *Entry) { e.Documents[0].URL = "http://example.com/a.json" }, "not an https URL"},
		"a URL with credentials":                      {func(e *Entry) { e.Documents[0].URL = "https://u:p@example.com/a.json" }, "not an https URL"},
		"a default given a definition":                {func(e *Entry) { e.Terms[0].Word = "read" }, "means one thing everywhere"},
		"a term a sentence reads as a keyword":        {func(e *Entry) { e.Terms[0].Word = "comment on issue" }, `which a sentence reads as a keyword`},
		"a term with no definition":                   {func(e *Entry) { e.Terms[0].Definition = " " }, "definition is empty"},
		"a definition with a control character":       {func(e *Entry) { e.Terms[0].Definition = "an \x1b[2Jissue" }, "control character"},
		"a term offered twice":                        {func(e *Entry) { e.Terms[1].Word = "issue" }, `"issue" is a type twice`},
		"a statement with a word the entry lacks":     {func(e *Entry) { e.Statements[0].ResourceType = "gist" }, `"gist" is not one of the entry's types`},
		"an action offered only as a type":            {func(e *Entry) { e.Statements[0].Action = "issue" }, `action "issue" is not a default`},
		"manage as a statement's action":              {func(e *Entry) { e.Statements[0].Action = policy.ActionManage }, `"manage" is every action, not one`},
		"anything as a statement's type":              {func(e *Entry) { e.Statements[0].ResourceType = policy.Anything }, "names no one type"},
		"a statement listed twice":                    {func(e *Entry) { e.Statements[1].Action = "create" }, "is listed twice"},
		"a statement that covers no operation":        {func(e *Entry) { e.Statements[0].Operations = nil }, "at least one documented operation"},
	} {
		t.Run(name, func(t *testing.T) {
			e := github()
			tc.edit(&e)
			err := e.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestVersionNamesWhatAnEntrySays(t *testing.T) {
	base := github().Version()
	if !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("Version() = %q", base)
	}

	reordered := github()
	reordered.Documents[0], reordered.Documents[1] = reordered.Documents[1], reordered.Documents[0]
	reordered.Terms[0], reordered.Terms[4] = reordered.Terms[4], reordered.Terms[0]
	if got := reordered.Version(); got != base {
		t.Errorf("reordering moved the version: %s, want %s", got, base)
	}

	shaped := github()
	shaped.Statements = shaped.Statements[1:]
	if got := shaped.Version(); got != base {
		t.Errorf("a statement removed moved the version, which would orphan every other statement's rules")
	}

	moved := github()
	moved.Documents[0].URL = "https://example.com/mirror/api.github.com.json"
	if got := moved.Version(); got != base {
		t.Errorf("a new URL for the same text moved the version")
	}

	for name, edit := range map[string]func(*Entry){
		"another document text": func(e *Entry) { e.Documents[0].SHA256 = strings.Repeat("b", 64) },
		"another definition":    func(e *Entry) { e.Terms[0].Definition = "Something else." },
		"another host":          func(e *Entry) { e.Host = "ghe.example.com" },
	} {
		e := github()
		edit(&e)
		if e.Version() == base {
			t.Errorf("%s left the version alone", name)
		}
	}
}

func str(s string) *string { return &s }

func TestLookup(t *testing.T) {
	e := github()
	repo := policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo}
	for name, tc := range map[string]struct {
		statement policy.Statement
		matched   map[string]int
		missing   []string
	}{
		"an action with a known shape, whatever its name and condition": {
			policy.Statement{Effect: policy.Allow, Actions: []string{"create"}, ResourceType: "issue", Name: str("1"), Scope: repo, When: str("its title starts with bug")},
			map[string]int{"create": 1}, nil,
		},
		"each action on its own": {
			policy.Statement{Effect: policy.Allow, Actions: []string{"create", "read", "delete"}, ResourceType: "issue", Scope: repo},
			map[string]int{"create": 1, "read": 1}, []string{"delete"},
		},
		"parents are part of the shape": {
			policy.Statement{Effect: policy.Allow, Actions: []string{"create"}, ResourceType: "comment", Scope: repo},
			nil, []string{"create"},
		},
		"with its parents": {
			policy.Statement{Effect: policy.Allow, Actions: []string{"create"}, ResourceType: "comment", Within: []policy.Resource{{Type: "issue"}}, Scope: repo},
			map[string]int{"create": 1}, nil,
		},
		"another scope type": {
			policy.Statement{Effect: policy.Allow, Actions: []string{"create"}, ResourceType: "issue", Scope: policy.Scope{Type: "organization", Name: "acme"}},
			nil, []string{"create"},
		},
		"manage is every shape of the type": {
			policy.Statement{Effect: policy.Allow, Actions: []string{"manage"}, ResourceType: "issue", Scope: repo},
			map[string]int{"manage": 2}, nil,
		},
		"anything anywhere": {
			policy.Statement{Effect: policy.Deny, Actions: []string{"create"}, ResourceType: policy.Anything, Scope: policy.Scope{Type: policy.Anywhere}},
			map[string]int{"create": 2}, nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := e.Lookup(tc.statement)
			matched := map[string]int{}
			for _, m := range got.Matches {
				matched[m.Action] = len(m.Shapes)
			}
			if len(matched) != len(tc.matched) {
				t.Fatalf("matches = %v, want %v", matched, tc.matched)
			}
			for action, n := range tc.matched {
				if matched[action] != n {
					t.Errorf("%s matched %d shapes, want %d", action, matched[action], n)
				}
			}
			if strings.Join(got.Missing, ",") != strings.Join(tc.missing, ",") {
				t.Errorf("missing = %v, want %v", got.Missing, tc.missing)
			}
			if got.Known() != (len(tc.missing) == 0) {
				t.Errorf("Known() = %v", got.Known())
			}
		})
	}
}

func TestForPicksTheNarrowestEntry(t *testing.T) {
	entries := []Entry{{Host: "github.com"}, {Host: "api.github.com"}, {Host: "example.com"}}
	for host, want := range map[string]string{
		"github.com":         "github.com",
		"uploads.github.com": "github.com",
		"API.github.com:443": "api.github.com",
		"x.api.github.com":   "api.github.com",
		"notgithub.com":      "",
	} {
		got, ok := For(entries, host)
		if got.Host != want || ok != (want != "") {
			t.Errorf("For(%q) = %q, %v; want %q", host, got.Host, ok, want)
		}
	}
}
