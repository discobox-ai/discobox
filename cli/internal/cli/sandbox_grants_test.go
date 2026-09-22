package cli

import (
	"strings"
	"testing"
)

// --grant names a use of a secret for the new discobox. Repeating a secret,
// host, and variable gives that one grant several uses; anything that is not
// SECRET[@HOST]:ENV_VAR=USE is refused with the shape it should have had.
func TestGrantFlagsBecomeTheNewDiscoboxsGrants(t *testing.T) {
	grants, err := sandboxGrants([]string{
		"github@github.com:GH_TOKEN=push a branch to org/repo",
		"npm:NPM_TOKEN=publish @org/pkg",
		"github@github.com:GH_TOKEN=open a pull request = the fix",
	})
	if err != nil {
		t.Fatalf("sandboxGrants: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %+v, want two", grants)
	}
	gh := grants[0]
	if gh.SecretId.Or("") != "github" || gh.EnvVar.Or("") != "GH_TOKEN" || gh.Host.Or("") != "github.com" || len(gh.Uses) != 2 ||
		gh.Uses[0].Description != "push a branch to org/repo" || gh.Uses[1].Description != "open a pull request = the fix" {
		t.Fatalf("github grant = %+v, want both uses on one grant, in order", gh)
	}
	if npm := grants[1]; npm.SecretId.Or("") != "npm" || npm.Host.IsSet() || len(npm.Uses) != 1 {
		t.Fatalf("npm grant = %+v, want the secret's own host", npm)
	}

	for _, bad := range []string{"github", "github:GH_TOKEN", ":GH_TOKEN=push", "github:=push", "github:GH_TOKEN= ", "=push", "@github.com=push"} {
		if _, err := sandboxGrants([]string{bad}); err == nil || !strings.Contains(err.Error(), "ID[@HOST]=USE") {
			t.Fatalf("--grant %q: err = %v, want the shape it should have had", bad, err)
		}
	}
}

// Without a variable, a --grant names a well-known credential by its ID, which
// says its own secret and variable; the host may still be narrowed.
func TestAGrantFlagMayNameAWellKnownID(t *testing.T) {
	grants, err := sandboxGrants([]string{
		"com.github.api=push a branch to org/repo",
		"com.github.api@api.github.com=read issues in org/repo",
		"com.github.api=open a pull request",
	})
	if err != nil {
		t.Fatalf("sandboxGrants: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %+v, want the site's grant and the narrower one", grants)
	}
	site, narrow := grants[0], grants[1]
	if site.WellKnownId.Or("") != "com.github.api" || site.SecretId.IsSet() || site.EnvVar.IsSet() || site.Host.IsSet() || len(site.Uses) != 2 {
		t.Fatalf("site grant = %+v, want the ID alone, with both of its uses", site)
	}
	if narrow.WellKnownId.Or("") != "com.github.api" || narrow.Host.Or("") != "api.github.com" || len(narrow.Uses) != 1 {
		t.Fatalf("narrow grant = %+v, want the ID narrowed to api.github.com", narrow)
	}
}
