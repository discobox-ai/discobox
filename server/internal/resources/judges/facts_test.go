package judges

import (
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// The facts come from what the row recorded at create: a local checkout is
// stated by the remote its client reported, never by its path on the client,
// and the other sources follow in an order that does not change between jobs.
// Nothing the discobox reports about itself since is read.
func TestTheDiscoboxIsReadFromWhatItWasCreatedWith(t *testing.T) {
	t.Parallel()
	sandbox := &model.Sandbox{
		ID:   "sbx_worker",
		Name: "approved-for-everything",
		SandboxManifest: model.SandboxManifest{
			Source: &model.GitSource{
				Kind: "git", LocalDirectory: new("/home/someone/src/repo"),
				UpstreamURL: new("git@github.com:org/repo.git"),
				Checkout:    &model.GitSourceCheckout{RefType: new("branch"), RefName: new("feature"), Commit: new("0123456789abcdef0123456789abcdef01234567")},
			},
			SourceCodeReferences: model.SourceCodeReferences{
				"/src/zeta":  {Kind: "git", URL: new("https://github.com/org/zeta")},
				"/src/alpha": {Kind: "git", URL: new("https://github.com/org/alpha")},
			},
		},
		AppliedCommits: []model.AppliedSourceCommit{{Slug: "primary"}},
	}
	want := []string{
		"The discobox asking is sbx_worker.",
		"The primary source sbx_worker was created with is the Git repository github.com/org/repo, checked out from the branch feature.",
		"Another source sbx_worker was created with is the Git repository github.com/org/alpha.",
		"Another source sbx_worker was created with is the Git repository github.com/org/zeta.",
	}
	if got := judge.Facts(discobox(sandbox)); !slices.Equal(got, want) {
		t.Fatalf("facts =\n%q\nwant\n%q", got, want)
	}
}
