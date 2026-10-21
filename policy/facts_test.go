package policy

import (
	"reflect"
	"testing"
)

// gitHub is a GitHub repository as a fact, or nil for none.
func gitHub(repo string) *Remote {
	if repo == "" {
		return nil
	}
	return &Remote{Host: "github.com", Repo: repo, URL: "https://github.com/" + repo}
}

// discobox is a discobox created from the GitHub repository primary, or from
// one with no network remote when primary is empty.
func discobox(id string, primary string) Facts {
	return Facts{
		Discobox: Discobox{ID: id, Project: "default"},
		Sources: []Source{
			{Slug: "primary", Primary: true, Remote: gitHub(primary), Ref: &Ref{Type: "branch", Name: "main"}},
			{Slug: "docs", Remote: gitHub("acme/docs")},
		},
	}
}

func TestFactsResolveTheVariables(t *testing.T) {
	f := discobox("sbx_1", "acme/web")
	for variable, want := range map[string]string{
		VarThis:               "sbx_1",
		VarPrimaryRepo:        "acme/web",
		VarPrimaryHost:        "github.com",
		VarPrimaryBranch:      "main",
		SourceRepoVar("docs"): "acme/docs",
	} {
		if got, ok := f.Value(variable); !ok || got != want {
			t.Errorf("Value(%s) = %q, %v; want %q", variable, got, ok, want)
		}
	}
}

func TestAVariableWithNoValueResolvesToNothing(t *testing.T) {
	tagged := discobox("sbx_1", "acme/web")
	tagged.Sources[0].Ref = &Ref{Type: "tag", Name: "v1"}
	local := discobox("sbx_1", "")
	for _, tc := range []struct {
		name     string
		facts    Facts
		variable string
	}{
		{"no primary source", Facts{Discobox: Discobox{ID: "sbx_1"}}, VarPrimaryRepo},
		{"a primary with no network remote", local, VarPrimaryRepo},
		{"a primary with no network remote, host", local, VarPrimaryHost},
		{"a primary created from a tag", tagged, VarPrimaryBranch},
		{"no such source", tagged, SourceRepoVar("nope")},
		{"an observed fact nobody observed", tagged, VarPrimaryUpstream},
		{"an observed default branch", tagged, VarPrimaryDefaultBranch},
		{"no discobox ID", Facts{}, VarThis},
		{"not a variable", tagged, "$primary.owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := tc.facts.Value(tc.variable); ok {
				t.Fatalf("Value(%s) = %q, want no value", tc.variable, got)
			}
		})
	}
}

// Every variable is listed, resolved or not, each with the value Resolve
// records for it, so what an agent is shown is what approval resolves.
func TestVariablesListEveryVariableAndWhatItResolvesTo(t *testing.T) {
	f := discobox("sbx_1", "acme/web")
	f.Sources = append(f.Sources, Source{Slug: "scratch"}, Source{})
	want := []Variable{
		{Name: VarThis, Value: "sbx_1", Resolved: true},
		{Name: VarPrimaryRepo, Value: "acme/web", Resolved: true},
		{Name: VarPrimaryHost, Value: "github.com", Resolved: true},
		{Name: VarPrimaryBranch, Value: "main", Resolved: true},
		{Name: VarPrimaryUpstream},
		{Name: VarPrimaryDefaultBranch},
		{Name: SourceRepoVar("primary"), Value: "acme/web", Resolved: true},
		{Name: SourceRepoVar("docs"), Value: "acme/docs", Resolved: true},
		{Name: SourceRepoVar("scratch")},
	}
	got := f.Variables()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Variables() =\n%+v\nwant\n%+v", got, want)
	}
	all := Policy{Statements: []Statement{{Effect: Allow, Actions: []string{"read"}, ResourceType: "anything", Scope: Scope{Type: "repository", Name: VarPrimaryRepo}}}}
	for _, v := range got {
		all.Statements[0].Scope.Name = v.Name
		resolved, ok := all.Resolve(f).Resolved[v.Name]
		if ok != v.Resolved || resolved != v.Value {
			t.Errorf("Resolve(%s) = %q, %v; Variables says %q, %v", v.Name, resolved, ok, v.Value, v.Resolved)
		}
	}
}

func TestResolveRecordsOnlyWhatResolves(t *testing.T) {
	p := pullRequests()
	p.Statements[0].Name = ptr(VarPrimaryUpstream)
	got := p.Resolve(discobox("sbx_1", "acme/web"))
	if want := map[string]string{VarPrimaryRepo: "acme/web"}; !reflect.DeepEqual(got.Resolved, want) {
		t.Fatalf("Resolved = %v, want %v", got.Resolved, want)
	}
	if p.Resolved != nil {
		t.Fatal("Resolve changed the policy it was called on")
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("a resolved policy does not validate: %v", err)
	}
}

func TestBindMatchesOnlyTheValueThatWasApproved(t *testing.T) {
	p := pullRequests()
	p.Statements[0].Name = ptr(VarPrimaryUpstream)
	approved := p.Resolve(discobox("sbx_1", "acme/web"))

	t.Run("unchanged", func(t *testing.T) {
		b := approved.Bind(discobox("sbx_1", "acme/web"))[VarPrimaryRepo]
		if v, ok := b.Value(); !ok || v != "acme/web" {
			t.Fatalf("Value = %q, %v; want acme/web", v, ok)
		}
	})
	t.Run("changed since approval", func(t *testing.T) {
		b := approved.Bind(discobox("sbx_1", "acme/other"))[VarPrimaryRepo]
		if v, ok := b.Value(); ok {
			t.Fatalf("a changed value still matches %q", v)
		}
		if *b.Approved != "acme/web" || *b.Current != "acme/other" {
			t.Fatalf("binding = %+v", b)
		}
	})
	t.Run("no longer resolves", func(t *testing.T) {
		b := approved.Bind(discobox("sbx_1", ""))[VarPrimaryRepo]
		if _, ok := b.Value(); ok || b.Current != nil {
			t.Fatalf("binding = %+v, want no current value", b)
		}
	})
	t.Run("did not resolve at approval", func(t *testing.T) {
		b := approved.Bind(discobox("sbx_1", "acme/web"))[VarPrimaryUpstream]
		if _, ok := b.Value(); ok || b.Approved != nil {
			t.Fatalf("binding = %+v, want nothing approved", b)
		}
	})
	t.Run("never approved", func(t *testing.T) {
		b := p.Bind(discobox("sbx_1", "acme/web"))[VarPrimaryRepo]
		if _, ok := b.Value(); ok {
			t.Fatal("a policy nobody approved bound a value")
		}
	})
}

// A policy delegated to another discobox resolves against, and binds to, the
// discobox it was handed to: $this is the receiver, not the giver.
func TestADelegatedPolicyBindsToTheReceivingDiscobox(t *testing.T) {
	p := pullRequests()
	p.Statements[0].CreatedBy = VarThis
	giver := discobox("sbx_giver", "acme/web")
	receiver := discobox("sbx_receiver", "acme/fork")
	receiver.Discobox.CreatedBy = "sbx_giver"

	delegated := p.Resolve(receiver)
	if got := delegated.Resolved[VarThis]; got != "sbx_receiver" {
		t.Fatalf("$this resolved to %q, want the receiver", got)
	}
	if v, ok := delegated.Bind(receiver)[VarPrimaryRepo].Value(); !ok || v != "acme/fork" {
		t.Fatalf("$primary.repo bound to %q, %v at the receiver", v, ok)
	}
	if _, ok := delegated.Bind(giver)[VarThis].Value(); ok {
		t.Fatal("a policy approved for the receiver bound $this at the giver")
	}
}

func TestBoundWritesEachVariableAsTheValueItMatches(t *testing.T) {
	p := pullRequests()
	p.Statements = append(p.Statements, Statement{
		Effect: Allow, Actions: []string{ActionRead}, ResourceType: "issue comment",
		Within: []Resource{{Type: "issue", Name: ptr("43"), CreatedBy: VarThis}},
		Scope:  Scope{Type: "repository", Name: VarPrimaryRepo},
	})
	p.Definitions["issue"] = "A GitHub issue."
	p.Definitions["issue comment"] = "A comment on a GitHub issue."
	approved := p.Resolve(discobox("sbx_1", "acme/web"))
	got := approved.Bound(discobox("sbx_1", "acme/web"))
	if got.Resolved != nil {
		t.Fatalf("Resolved = %v, want none", got.Resolved)
	}
	want := []string{
		`allow create, update on pull request in repository "acme/web" when "its head is discobox/issue-46 and its base is main"`,
		`deny force-push on branch in anywhere`,
		`allow read on issue comment within issue "43" created by "sbx_1" in repository "acme/web"`,
	}
	if sentences := FormatStatements(got.Statements); !reflect.DeepEqual(sentences, want) {
		t.Fatalf("Bound = %q, want %q", sentences, want)
	}
	// Bound, a statement names the discobox it was created by, which
	// Validate refuses; its definitions still fit it.
	if err := got.ValidateDefinitions(); err != nil {
		t.Errorf("ValidateDefinitions() error = %v", err)
	}
	if again := FormatStatements(approved.Statements); again[0] != `allow create, update on pull request in repository $primary.repo when "its head is discobox/issue-46 and its base is main"` ||
		*approved.Statements[2].Within[0].Name != "43" || approved.Statements[2].Within[0].CreatedBy != VarThis {
		t.Fatalf("Bound changed the policy it was called on: %q", again)
	}
}

// A variable that does not bind matches nothing: an allow naming one covers
// nothing and is left out, and a deny naming one is kept as written, because
// leaving a deny out could only widen what a reader allows.
func TestBoundLeavesOutAnAllowWhoseVariableDoesNotBind(t *testing.T) {
	p := pullRequests()
	p.Statements[1].Scope = Scope{Type: "repository", Name: VarPrimaryUpstream}
	approved := p.Resolve(discobox("sbx_1", "acme/web"))
	for name, facts := range map[string]Facts{
		"no longer resolves":       discobox("sbx_1", ""),
		"changed since approval":   discobox("sbx_1", "acme/other"),
		"resolved for nobody here": {},
	} {
		t.Run(name, func(t *testing.T) {
			got := approved.Bound(facts)
			if sentences := FormatStatements(got.Statements); !reflect.DeepEqual(sentences, []string{`deny force-push on branch in repository $primary.upstream`}) {
				t.Fatalf("Bound = %q, want the deny as written", sentences)
			}
			// The allow's words went with it; the deny's stay defined.
			if want := map[string]string{"force-push": p.Definitions["force-push"], "branch": p.Definitions["branch"], "repository": p.Definitions["repository"]}; !reflect.DeepEqual(got.Definitions, want) {
				t.Fatalf("Definitions = %v, want %v", got.Definitions, want)
			}
		})
	}
}
