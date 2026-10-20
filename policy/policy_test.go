package policy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func ptr(s string) *string { return &s }

// pullRequests is the ADR's example: create and update pull requests in the
// primary repository, from one branch into main.
func pullRequests() Policy {
	return Policy{
		Statements: []Statement{
			{
				Effect: Allow, Actions: []string{ActionCreate, ActionUpdate}, ResourceType: "pull request",
				Scope: Scope{Type: "repository", Name: VarPrimaryRepo},
				When:  ptr("its head is discobox/issue-46 and its base is main"),
			},
			{
				Effect: Deny, Actions: []string{"force-push"}, ResourceType: "branch",
				Scope: Scope{Type: Anywhere},
			},
		},
		Definitions: map[string]string{
			"pull request": "A GitHub pull request.",
			"repository":   "A GitHub repository, owner/name.",
			"force-push":   "Push a ref so that commits it pointed at are no longer on it.",
			"branch":       "A Git branch.",
		},
	}
}

func TestAPolicyRoundTripsThroughJSON(t *testing.T) {
	want := pullRequests().Resolve(Facts{Sources: []Source{{Slug: "primary", Primary: true, Remote: gitHub("acme/web")}}})
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Policy
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !strings.Contains(string(data), `"resourceType":"pull request"`) || !strings.Contains(string(data), `"name":null`) {
		t.Fatalf("JSON is not the ADR's shape: %s", data)
	}
}

func TestValidateAcceptsTheDefaultsWithoutDefinitions(t *testing.T) {
	p := Policy{Statements: []Statement{{
		Effect: Allow, Actions: []string{ActionRead, ActionManage}, ResourceType: Anything,
		Scope: Scope{Type: "repository", Name: "acme/*"},
	}}, Definitions: map[string]string{"repository": "A GitHub repository."}}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Policy)
		want   string
	}{
		{"no statements", func(p *Policy) { p.Statements = nil }, "at least one statement"},
		{"an unknown effect", func(p *Policy) { p.Statements[0].Effect = "permit" }, `"permit", not allow or deny`},
		{"no actions", func(p *Policy) { p.Statements[0].Actions = nil }, "at least one action"},
		{"an action twice", func(p *Policy) { p.Statements[0].Actions = []string{"create", "create"} }, "named twice"},
		{"no resource type", func(p *Policy) { p.Statements[0].ResourceType = "" }, "resource type: empty"},
		{"an allow with no scope", func(p *Policy) { p.Statements[0].Scope = Scope{} }, "scope type: empty"},
		{"an allow anywhere", func(p *Policy) { p.Statements[0].Scope = Scope{Type: Anywhere} }, "only a deny may apply anywhere"},
		{"a scope with no name", func(p *Policy) { p.Statements[0].Scope.Name = "" }, `scope "repository" has no name`},
		{"an allow in anything", func(p *Policy) { p.Statements[0].Scope = Scope{Type: Anything, Name: "*"} }, `scope type "anything" names no scope`},
		{"a deny in anything", func(p *Policy) { p.Statements[1].Scope = Scope{Type: Anything, Name: "*"} }, `scope type "anything" names no scope`},
		{"a condition with an escape", func(p *Policy) { p.Statements[0].When = ptr("its base is main\x1b[2K") }, "when has a control character"},
		{"a definition with a newline", func(p *Policy) { p.Definitions["branch"] = "A Git branch.\nAnd anything else." }, `definition of "branch" has a control character`},
		{"anywhere with a name", func(p *Policy) { p.Statements[1].Scope.Name = "x" }, "anywhere has no name"},
		{"a word with no definition", func(p *Policy) { delete(p.Definitions, "pull request") }, `"pull request" is not a default word and has no definition`},
		{"a defined default", func(p *Policy) { p.Definitions["read"] = "read and write" }, `"read" means one thing everywhere`},
		{"a defined anything", func(p *Policy) { p.Definitions[Anything] = "everything" }, `"anything" means one thing everywhere`},
		{"an unused definition", func(p *Policy) { p.Definitions["issue"] = "A GitHub issue." }, `"issue" is defined but no statement uses it`},
		{"an empty definition", func(p *Policy) { p.Definitions["branch"] = " " }, `definition of "branch" is empty`},
		{"an empty when", func(p *Policy) { p.Statements[0].When = ptr("  ") }, "when is empty"},
		{"an empty unless", func(p *Policy) { p.Statements[0].Unless = ptr("") }, "unless is empty"},
		{"an unknown variable", func(p *Policy) { p.Statements[0].Scope.Name = "$primary.owner" }, "not a variable a policy may use"},
		{"a variable inside a name", func(p *Policy) { p.Statements[0].Name = ptr("discobox/$this") }, "a variable is a whole name"},
		{"a bad glob", func(p *Policy) { p.Statements[0].Name = ptr("[a") }, "not a glob"},
		{"created by someone else", func(p *Policy) { p.Statements[0].CreatedBy = "sbx_1" }, `"sbx_1" is not $this`},
		{"a variable as a type", func(p *Policy) { p.Statements[0].ResourceType = "$this" }, "is a variable, not a word"},
		{"a resolved variable no statement uses", func(p *Policy) { p.Resolved = map[string]string{VarThis: "sbx_1"} }, `"$this" is resolved but no statement uses it`},
		{"a parent with no definition", func(p *Policy) {
			p.Statements[0].Within = []Resource{{Type: "project"}}
		}, `"project" is not a default word`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pullRequests()
			tc.mutate(&p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestVariablesListsEveryVariableUsedOnce(t *testing.T) {
	p := pullRequests()
	p.Statements[0].Name = ptr(VarPrimaryBranch)
	p.Statements[0].CreatedBy = VarThis
	p.Statements[0].Within = []Resource{{Type: "fork", Name: ptr(SourceRepoVar("docs"))}}
	p.Statements = append(p.Statements, p.Statements[0])
	want := []string{VarPrimaryBranch, VarPrimaryRepo, VarThis, "$sources.docs.repo"}
	if got := p.Variables(); !reflect.DeepEqual(got, sortStrings(want)) {
		t.Fatalf("Variables = %v, want %v", got, sortStrings(want))
	}
}

func sortStrings(s []string) []string {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return sortedKeys(m)
}

func TestAStatementAloneValidatesButForDefinitions(t *testing.T) {
	repo := Scope{Type: "repository", Name: VarPrimaryRepo}
	if err := (Statement{Effect: Allow, Actions: []string{"push"}, ResourceType: "branch", Scope: repo}).Validate(); err != nil {
		t.Fatalf("a statement whose words have no definitions yet: %v", err)
	}
	if err := (Statement{Effect: Allow, Actions: []string{ActionRead}, ResourceType: "issue", Scope: Scope{Type: Anywhere}}).Validate(); err == nil {
		t.Fatal("an allow anywhere validated")
	}
}
