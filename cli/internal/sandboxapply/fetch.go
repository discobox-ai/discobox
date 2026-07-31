// Package sandboxapply implements ADR 0014's disco apply: pulling a
// sandbox's committed source changes into the host repository they started
// from.
package sandboxapply

import (
	"context"
	"fmt"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
	"github.com/obot-platform/discobox/internal/gitutil"
)

// FetchRef is the discobox-owned ref a source's sandbox commits land under
// after FetchSource, mirroring the refs/discobox/run/<id> convention used for
// dirty-workspace snapshots at create.
func FetchRef(sandboxID, slug string) string {
	return "refs/discobox/apply/" + sandboxID + "/" + slug
}

// FetchSource fetches source's sandbox repository into repoRoot, landing its
// current tip at FetchRef(sandboxID, slug), and returns that tip's commit
// SHA.
//
// It reuses the exact URL and bearer-token auth construction
// cli/internal/sandboxcreate uses to push a source at create
// (SandboxGitRepositoryURL, GitAuthArgs) — the git-repositories proxy already
// grants fetch under ScopeSandboxRead, so no new server capability is needed,
// only the read direction of the same transport (ADR 0014 §1).
func FetchSource(ctx context.Context, repoRoot, serverURL, projectID, sandboxID, token string, source apimodel.GitSource) (string, error) {
	slug, ok := source.Slug.Get()
	if !ok || slug == "" {
		return "", fmt.Errorf("source has no slug to address its repository")
	}
	ref := FetchRef(sandboxID, slug)
	if err := Fetch(ctx, repoRoot, serverURL, projectID, sandboxID, token, source, "+HEAD:"+ref); err != nil {
		return "", err
	}
	tip, err := gitutil.ResolveCommit(ctx, repoRoot, ref)
	if err != nil {
		return "", fmt.Errorf("resolve fetched tip for source %q: %w", slug, err)
	}
	return tip, nil
}

// Fetch brings refspec from a source's sandbox repository into repoRoot.
//
// The proxy is a transparent reverse proxy over the sandbox's own git HTTP
// endpoint, and everything that is not receive-pack is served under
// ScopeSandboxRead, so upload-pack advertises every ref the sandbox has. Any
// ref the sandbox creates is therefore fetchable with no new server capability
// — which is what lets `disco diff --base local` bring the sandbox's working
// state to this machine.
func Fetch(ctx context.Context, repoRoot, serverURL, projectID, sandboxID, token string, source apimodel.GitSource, refspec string) error {
	slug := source.Slug.Or("")
	repoURL, err := sandboxcreate.SandboxGitRepositoryURL(serverURL, projectID, sandboxID, source)
	if err != nil {
		return err
	}
	args := sandboxcreate.GitAuthArgs(token, []string{"fetch", repoURL, refspec})
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, args...); err != nil {
		return fmt.Errorf("fetch source %q from sandbox: %w", slug, err)
	}
	return nil
}
