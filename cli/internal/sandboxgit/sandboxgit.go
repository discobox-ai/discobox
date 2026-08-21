// Package sandboxgit builds the client's git transport to a sandbox: the URLs of
// the two repositories the control plane proxies, the authentication carried on
// those requests, and the client-side refs that record what has been sent.
//
// It is shared by every direction that transport runs in — create's delivery
// push (ADR 0001), `discobox apply`'s fetch (ADR 0014), and `discobox push`'s re-push
// (ADR 0058) — so all three address a sandbox the same way.
package sandboxgit

import (
	"fmt"
	"net/url"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

// SourcePushBranch is the branch a push-delivered source's commits land on when
// the source names no branch of its own — a checkout at a bare commit or tag. The
// sandbox checks out the commit itself; this only gives the push somewhere to
// land, and gives the origin repository a HEAD to point at.
const SourcePushBranch = "discobox-source"

// RepositoryURL is the control plane's proxy to a sandbox's own repository — the
// worktree the sandbox works in. The request goes through the server rather than
// to the sandbox: the sandbox sits on a private network the client cannot reach.
func RepositoryURL(serverURL, projectID, sandboxID string, source apimodel.GitSource) (string, error) {
	return repositoryURL(serverURL, projectID, sandboxID, "git-repositories", source)
}

// OriginURL is the proxy to the origin repository of a push-delivered source:
// the bare repository the client pushes into and the sandbox fetches from (ADR
// 0058 §3). It is a different repository from RepositoryURL's, on its own route.
func OriginURL(serverURL, projectID, sandboxID string, source apimodel.GitSource) (string, error) {
	return repositoryURL(serverURL, projectID, sandboxID, "git-origins", source)
}

func repositoryURL(serverURL, projectID, sandboxID, kind string, source apimodel.GitSource) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(serverURL), "/")
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("parse server URL %q: %w", serverURL, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		// git only speaks URLs, so callers bridge a unix socket or named pipe
		// endpoint to a loopback HTTP address before they get here. Reaching
		// this means a caller passed the raw endpoint instead.
		return "", fmt.Errorf("cannot reach the discobox repository at server endpoint %q: an HTTP endpoint is required", serverURL)
	}
	slug := strings.TrimSpace(source.Slug.Or(""))
	if slug == "" {
		return "", fmt.Errorf("discobox source has no slug to address its repository")
	}
	return fmt.Sprintf("%s/projects/%s/sandboxes/%s/%s/%s.git",
		base, url.PathEscape(projectID), url.PathEscape(sandboxID), kind, url.PathEscape(slug)), nil
}

// AuthArgs carries the caller's bearer token on the request. It is passed as a
// git config override rather than embedded in the URL, which would put the token
// in the repository's remote configuration and in process listings.
func AuthArgs(token string, args []string) []string {
	token = strings.TrimSpace(token)
	if token == "" {
		return args
	}
	return append([]string{"-c", "http.extraHeader=Authorization: Bearer " + token}, args...)
}

// OriginLeaseRef is where a client records the commit it last pushed to one
// branch of a source's origin repository, in its own repository.
//
// It is what makes a re-push a lease rather than a blind force: the question
// worth asking is not "is the origin where I just read it" but "has anything
// moved it since I last pushed" — which only this client can answer, and only by
// remembering (ADR 0058 §6). It follows the refs/discobox convention used for
// dirty-workspace snapshots and applied commits.
//
// The branch is part of the ref because the lease protects one ref: a client that
// also pushes a second branch for the sandbox to rebase onto has said nothing
// about where the first one should be. Two branch names that cannot coexist in
// refs/heads cannot coexist here either, which is the same constraint git already
// enforces on the branches themselves.
func OriginLeaseRef(sandboxID, slug, branch string) string {
	return "refs/discobox/origin/" + sandboxID + "/" + slug + "/" + branch
}
