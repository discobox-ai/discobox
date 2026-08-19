package sandboxgit

import (
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func sourceFixture(slug string) apimodel.GitSource {
	source := apimodel.GitSource{}
	if slug != "" {
		source.Slug = apiclientgen.NewOptString(slug)
	}
	return source
}

func TestRepositoryURL(t *testing.T) {
	source := sourceFixture("primary")

	got, err := RepositoryURL("https://disco.example.com/", "proj_1", "sbx_1", source)
	if err != nil {
		t.Fatalf("RepositoryURL: %v", err)
	}
	want := "https://disco.example.com/projects/proj_1/sandboxes/sbx_1/git-repositories/primary.git"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	// A local server binds the source instead of asking for a push, so a socket
	// endpoint here means the client and server disagree about reachability.
	if _, err := RepositoryURL("unix:///run/discobox.sock", "proj_1", "sbx_1", source); err == nil {
		t.Fatal("pushing to a unix endpoint: got nil error, want failure")
	}

	if _, err := RepositoryURL("https://disco.example.com", "proj_1", "sbx_1", sourceFixture("")); err == nil {
		t.Fatal("source with no slug: got nil error, want failure")
	}
}

// The origin repository is a different repository from the worktree, on its own
// route, so that a slug can never collide with a synthesized repository id.
func TestOriginURLIsItsOwnRoute(t *testing.T) {
	got, err := OriginURL("https://disco.example.com", "proj_1", "sbx_1", sourceFixture("primary"))
	if err != nil {
		t.Fatalf("OriginURL: %v", err)
	}
	want := "https://disco.example.com/projects/proj_1/sandboxes/sbx_1/git-origins/primary.git"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	worktree, err := RepositoryURL("https://disco.example.com", "proj_1", "sbx_1", sourceFixture("primary"))
	if err != nil {
		t.Fatalf("RepositoryURL: %v", err)
	}
	if got == worktree {
		t.Fatalf("origin and worktree URLs are the same: %q", got)
	}
}

// The token must ride on the request header, not in the URL, which would land
// it in the repository's remote config and in process listings.
func TestAuthArgs(t *testing.T) {
	args := AuthArgs("secret-token", []string{"push", "https://disco.example.com/x.git"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-c http.extraHeader=Authorization: Bearer secret-token") {
		t.Fatalf("args = %v, want the token as an extraHeader config override", args)
	}
	if strings.Contains(args[len(args)-1], "secret-token") {
		t.Fatalf("token leaked into the push URL: %v", args)
	}

	plain := AuthArgs("  ", []string{"push", "url"})
	if len(plain) != 2 {
		t.Fatalf("args = %v, want no auth override without a token", plain)
	}
}

// The lease is per origin ref, so pushing a second branch for the sandbox to
// rebase onto says nothing about where the first one should be.
func TestOriginLeaseRefIsPerBranch(t *testing.T) {
	if got, want := OriginLeaseRef("sbx_1", "primary", "main"), "refs/discobox/origin/sbx_1/primary/main"; got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
	if OriginLeaseRef("sbx_1", "primary", "main") == OriginLeaseRef("sbx_1", "primary", "spike") {
		t.Fatal("two branches share one lease ref")
	}
}
