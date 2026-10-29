package sandboxcreate

import (
	"context"
	"strings"
	"testing"
)

// --bind names a secret, the variable the new discobox receives it in, and
// routes that narrow it; --delegate names a secret the new discobox may give
// on. Anything else is refused with the shape it should have had.
func TestBindAndDelegateFlagsBecomeTheNewDiscoboxsBindings(t *testing.T) {
	got, err := ParseBindings(
		[]string{"github:GH_TOKEN@api.github.com,*.github.com,gist.github.com=blocked", "npm:NPM_TOKEN"},
		[]string{"github@api.github.com"},
	)
	if err != nil {
		t.Fatalf("ParseBindings: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("bindings = %+v, want three", got)
	}
	gh := got[0]
	if gh.Secret != "github" || gh.EnvVar.Or("") != "GH_TOKEN" || len(gh.Routes) != 3 ||
		gh.Routes[0].Hosts[0] != "api.github.com" || gh.Routes[1].Hosts[0] != "*.github.com" || gh.Routes[1].Blocked.Or(false) ||
		gh.Routes[2].Hosts[0] != "gist.github.com" || !gh.Routes[2].Blocked.Or(false) {
		t.Fatalf("github binding = %+v", gh)
	}
	if npm := got[1]; npm.Secret != "npm" || npm.Routes != nil {
		t.Fatalf("npm binding = %+v, want it to inherit the secret's routes", npm)
	}
	if d := got[2]; d.Secret != "github" || !d.Delegate.Or(false) || d.Use.Or(true) || d.EnvVar.Set {
		t.Fatalf("delegation = %+v, want delegate only, delivered nothing", d)
	}
	for _, bad := range []string{":GH_TOKEN", "github:", "github:GH TOKEN", "github:GH_TOKEN@github.com=read-only"} {
		if _, err := ParseBindings([]string{bad}, nil); err == nil || !strings.Contains(err.Error(), "--bind") {
			t.Fatalf("--bind %q: err = %v, want it refused", bad, err)
		}
	}
	if _, err := ParseBindings(nil, []string{"github:GH_TOKEN"}); err == nil {
		t.Fatal("--delegate with a variable was accepted")
	}
}

// One variable holds one credential, and a secret is delegated once.
func TestAVariableIsBoundOnce(t *testing.T) {
	if _, err := ParseBindings([]string{"github:GH_TOKEN", "other:GH_TOKEN"}, nil); err == nil || !strings.Contains(err.Error(), "GH_TOKEN is already bound") {
		t.Fatalf("err = %v, want the repeated variable refused", err)
	}
	if _, err := ParseBindings(nil, []string{"github", "github@x.com"}); err == nil {
		t.Fatal("a secret delegated twice was accepted")
	}
}

// `*` is a route to any host (ADR 26-10-27-939).
func TestABindingMayRouteAnywhere(t *testing.T) {
	got, err := ParseBindings([]string{"openai:OPENAI_API_KEY@*"}, nil)
	if err != nil || len(got[0].Routes) != 1 || got[0].Routes[0].Hosts[0] != "*" {
		t.Fatalf("bindings = %+v, %v; want one * route", got, err)
	}
}

// A --bind that names no variable binds the secret in its name, upper-snaked.
func TestABindWithoutAVariableUsesTheSecretsName(t *testing.T) {
	got, err := ParseBindings([]string{"npm-token", "github@api.github.com"}, nil)
	if err != nil {
		t.Fatalf("ParseBindings: %v", err)
	}
	if got[0].EnvVar.Or("") != "NPM_TOKEN" || got[1].EnvVar.Or("") != "GITHUB" || len(got[1].Routes) != 1 {
		t.Fatalf("bindings = %+v, want NPM_TOKEN and GITHUB", got)
	}
}

// A secret given by ID with no variable is named after the secret's name, not
// its ID, as `secret bind` names it.
func TestABindByIDIsNamedAfterTheSecret(t *testing.T) {
	const id = "sec_nfkv7n8v46gvn7t2"
	got, err := ParseBindings([]string{id}, nil)
	if err != nil {
		t.Fatalf("ParseBindings: %v", err)
	}
	if got[0].EnvVar.Set {
		t.Fatalf("binding = %+v, want the variable left for the lookup", got[0])
	}
	if err := NameBindingVariables(context.Background(), &fakeCreator{}, "project-1", got); err != nil {
		t.Fatalf("NameBindingVariables: %v", err)
	}
	if got[0].EnvVar.Or("") != "GITHUB_TOKEN" {
		t.Fatalf("variable = %q, want the secret's name upper-snaked", got[0].EnvVar.Or(""))
	}
}

// Each inline secret says where it is sent, so an @ read as a host is seen.
func TestAnInlineSecretSaysWhereItIsSent(t *testing.T) {
	_, secrets, err := EnvAndSecretsFromOptions(nil, []string{"DB_PASS=hunter2@corp.example", "OPENAI_KEY=sk"})
	if err != nil {
		t.Fatal(err)
	}
	if got := InlineSecretLine(secrets[0]); got != "DB_PASS: stored, sent to corp.example" {
		t.Fatalf("line = %q", got)
	}
	if got := InlineSecretLine(secrets[1]); got != "OPENAI_KEY: stored, sent anywhere" {
		t.Fatalf("line = %q", got)
	}
}
