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
