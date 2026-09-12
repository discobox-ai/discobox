// Package sandboxapply implements ADR 0014's discobox apply: pulling a
// sandbox's committed source changes into the host repository they started
// from.
package sandboxapply

import (
	"context"
	"fmt"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/x/gitutil"
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
// It reuses the exact URL and bearer-token auth construction the delivery push
// uses (sandboxgit.RepositoryURL, sandboxgit.AuthArgs) — the git-repositories
// proxy already grants fetch under ScopeSandboxRead, so no new server capability
// is needed, only the read direction of the same transport (ADR 0014 §1).
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
// — which is what lets `discobox apply` bring the sandbox's working state to
// this machine.
func Fetch(ctx context.Context, repoRoot, serverURL, projectID, sandboxID, token string, source apimodel.GitSource, refspec string) error {
	slug := source.Slug.Or("")
	repoURL, err := sandboxgit.RepositoryURL(serverURL, projectID, sandboxID, source)
	if err != nil {
		return err
	}
	args := sandboxgit.AuthArgs(token, []string{"fetch", repoURL, refspec})
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, args...); err != nil {
		return fmt.Errorf("fetch source %q from discobox: %w", slug, err)
	}
	return nil
}

// Tip is the commit at the HEAD of a source's sandbox repository, read over the
// same proxy Fetch uses but without bringing anything into a local repository:
// `git ls-remote` needs no repository to run in.
//
// It is what lets a source whose local directory is unknown still be asked the
// only question that matters about it — whether the sandbox has committed
// anything at all — before that unknown directory is treated as a problem.
func Tip(ctx context.Context, serverURL, projectID, sandboxID, token string, source apimodel.GitSource) (string, error) {
	slug := source.Slug.Or("")
	repoURL, err := sandboxgit.RepositoryURL(serverURL, projectID, sandboxID, source)
	if err != nil {
		return "", err
	}
	args := sandboxgit.AuthArgs(token, []string{"ls-remote", repoURL, "HEAD"})
	out, err := gitutil.Output(ctx, "", nil, nil, args...)
	if err != nil {
		return "", fmt.Errorf("read the tip of source %q in the discobox: %w", slug, err)
	}
	tip, ok := lsRemoteCommit(out, "HEAD")
	if !ok {
		return "", fmt.Errorf("source %q has no HEAD in the discobox", slug)
	}
	return tip, nil
}

// lsRemoteCommit picks the commit `git ls-remote` reported for one ref out of
// its output.
//
// It reads the ref lines rather than the first word of the output, because the
// output is not only ref lines: gitutil.Output combines stdout and stderr, and
// git writes to stderr on a remote operation whenever it has something to say
// — "warning: redirecting to <url>/" when the server answers a 30x is the
// routine one here. Taking the first token would return "warning:" as the
// commit, which no comparison can then match.
func lsRemoteCommit(out, ref string) (string, bool) {
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		// A ref line is exactly "<object id>\t<ref>". Anything git said to
		// stderr fails one of those two tests.
		if len(fields) != 2 || fields[1] != ref || !isObjectID(fields[0]) {
			continue
		}
		return fields[0], true
	}
	return "", false
}

// isObjectID reports whether a field is a full git object id: SHA-1 or SHA-256,
// which are the two hash lengths a repository can use.
func isObjectID(field string) bool {
	if len(field) != 40 && len(field) != 64 {
		return false
	}
	for _, r := range field {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
