package model_test

import (
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// A policy's facts are the record the judge's are: what the discobox was
// created with and by whom, typed. A local checkout is its reported remote,
// the other sources follow in destination order, and a remote that names no
// repository on a network is none.
func TestPolicyFactsAreTheRecordTyped(t *testing.T) {
	t.Parallel()
	sandbox := &model.Sandbox{
		ID:                 "sbx_worker",
		ProjectID:          "default",
		CreatedBySandboxID: new("sbx_lead"),
		SandboxManifest: model.SandboxManifest{
			Source: &model.GitSource{
				Kind: "git", Slug: new("primary"), LocalDirectory: new("/home/someone/src/repo"),
				UpstreamURL: new("git@github.com:org/repo.git"),
				Checkout:    &model.GitSourceCheckout{RefType: new("branch"), RefName: new("feature"), Commit: new("0123456789abcdef0123456789abcdef01234567")},
			},
			SourceCodeReferences: model.SourceCodeReferences{
				"/src/zeta":  {Kind: "git", Slug: new("zeta"), URL: new("https://github.com/org/zeta")},
				"/src/alpha": {Kind: "git", Slug: new("alpha"), LocalDirectory: new("/tmp/alpha")},
			},
		},
	}
	want := policy.Facts{
		Discobox: policy.Discobox{ID: "sbx_worker", CreatedBy: "sbx_lead", Project: "default"},
		Sources: []policy.Source{
			{
				Slug: "primary", Primary: true,
				Remote: &policy.Remote{Host: "github.com", Repo: "org/repo", URL: "https://github.com/org/repo"},
				Ref:    &policy.Ref{Type: "branch", Name: "feature"}, Commit: "0123456789abcdef0123456789abcdef01234567",
			},
			{Slug: "alpha"},
			{Slug: "zeta", Remote: &policy.Remote{Host: "github.com", Repo: "org/zeta", URL: "https://github.com/org/zeta"}},
		},
	}
	got := sandbox.PolicyFacts()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("facts =\n%+v\nwant\n%+v", got, want)
	}
	if repo, ok := got.Value(policy.SourceRepoVar("zeta")); !ok || repo != "org/zeta" {
		t.Fatalf("$sources.zeta.repo = %q, %v", repo, ok)
	}
}

// A remote is a fact only when it names a repository on a network: a pool's
// origins route resolves only inside the discobox that has it, and a path is
// nobody else's. The judge is told the same, since it reads these facts.
func TestARemoteThatNamesNoNetworkRepositoryIsNone(t *testing.T) {
	t.Parallel()
	sandbox := &model.Sandbox{
		ID: "sbx_1",
		SandboxManifest: model.SandboxManifest{
			Source: &model.GitSource{Kind: "git", URL: new("https://git.discobox.internal/api/project/p/pool/q/sandboxes/sbx_1/git-origins/primary.git")},
			SourceCodeReferences: model.SourceCodeReferences{
				"/src/a": {Kind: "git", URL: new("ssh://git@GitHub.com:22/acme/web.git")},
				"/src/b": {Kind: "git", LocalDirectory: new("/srv/git/web")},
			},
		},
	}
	facts := sandbox.PolicyFacts()
	if facts.Sources[0].Remote != nil || facts.Sources[2].Remote != nil {
		t.Fatalf("sources = %+v, want no remote for the origins route or the path", facts.Sources)
	}
	if got := facts.Sources[1].Remote; got == nil || *got != (policy.Remote{Host: "github.com", Repo: "acme/web", URL: "https://github.com/acme/web"}) {
		t.Fatalf("remote = %+v, want github.com acme/web", got)
	}
	if _, ok := facts.Value(policy.VarPrimaryRepo); ok {
		t.Fatal("$primary.repo resolved from a pool's origins route")
	}
}
