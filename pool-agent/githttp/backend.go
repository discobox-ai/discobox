// Package githttp serves a source's origin over git's smart HTTP protocol,
// through gitbackend: the bare repository made for one sandbox (ADR 0058), or
// a sandbox source's namespace of a pool's cache (ADR 26-10-10-545). The
// sandbox's own worktree is not served here: sandbox-agent serves it, and the
// pool forwards to it (ADR 0126 §4).
package githttp

import (
	"net/http"
	"os/exec"
	"slices"

	"github.com/discobox-ai/discobox/pkg/gitbackend"
	"github.com/discobox-ai/discobox/pool-agent/childproc"
	"github.com/discobox-ai/discobox/pool-agent/execidentity"
)

// Repository is what one request is served from: a directory git
// http-backend can open, the identity it runs as, and how much of it the
// caller may reach.
type Repository struct {
	Path string
	// UID and GID are the identity the backend runs as: the repository's
	// owner, so git's dubious-ownership check (safe.directory) does not reject
	// the request. A negative uid means "run as the calling process" (used when
	// there is no specific owner to impersonate).
	UID int
	GID int
	// Namespace is the Git namespace the request is served in, empty for a
	// repository served whole.
	Namespace string
	// FetchOnly serves a sandbox's own fetch of its origin and nothing else;
	// see ServeBackend.
	FetchOnly bool
	// HideNamespaces serves a cache's own refs and none of its namespaces'.
	HideNamespaces bool
	// Config and Env are added to the backend's switches and environment: how
	// a cache receives a push, which is the cache's to say.
	Config []string
	Env    []string
}

// ServeBackend runs git http-backend against repo, as its owner.
//
// A sandbox's own fetch (FetchOnly) is served under three restrictions that
// only hold together. Project members are trusted with what a pool holds; a
// sandbox runs code nobody has read, and its namespace shares an object store
// with every other sandbox of the same remote:
//
//   - Only the smart upload-pack service. http-backend also answers the dumb
//     protocol — loose objects and packs fetched as plain files — which
//     reaches every object in the repository whatever is advertised, so
//     anything but the two upload-pack endpoints is refused here and
//     http.getanyfile is off besides. Nothing a sandbox holds a token for
//     pushes: work leaves a sandbox through the worktree route.
//   - Protocol v0, whatever the client asks for. A v2 upload-pack serves any
//     object it is asked for by id, advertised or not and whatever namespace
//     it is in; v0 refuses a want that no advertised ref reaches.
//   - The uploadpack.allow*InWant switches off on the command line, where
//     nothing in the repository's own configuration can turn them back on:
//     each is a way to be served an object no advertised ref reaches.
func ServeBackend(w http.ResponseWriter, r *http.Request, repo Repository, suffix string) {
	backend := gitbackend.Backend{
		Root: repo.Path,
		// A bare origin takes the client's pushes (ADR 0058 §3).
		Config:      []string{"-c", "http.receivepack=true"},
		RemoteUser:  "pool-agent",
		SysProcAttr: execidentity.SysProcAttr(repo.UID, repo.GID),
		// Through childproc, so the pool agent's reaper leaves this backend's
		// exit status to gitbackend's Wait rather than collecting it first
		// (ADR 0087).
		Start: func(cmd *exec.Cmd) (gitbackend.Process, error) { return childproc.Start(cmd) },
	}
	backend.Config = append(backend.Config, repo.Config...)
	backend.Env = slices.Clone(repo.Env)
	if repo.Namespace != "" {
		backend.Env = append(backend.Env, "GIT_NAMESPACE="+repo.Namespace)
	}
	if repo.FetchOnly || repo.HideNamespaces {
		if !uploadPackRequest(r, suffix) {
			http.Error(w, "this repository is served fetch-only, over git's smart protocol", http.StatusForbidden)
			return
		}
		backend.Config = fetchOnlyArgs()
	}
	if repo.FetchOnly {
		backend.FixedProtocol = true
	}
	if repo.HideNamespaces {
		backend.Config = append(backend.Config, "-c", "uploadpack.hideRefs=refs/namespaces")
	}
	gitbackend.Serve(w, r, backend, suffix)
}

// uploadPackRequest reports whether r is one of the two requests a smart fetch
// makes, and nothing else http-backend would answer.
func uploadPackRequest(r *http.Request, suffix string) bool {
	switch suffix {
	case "/info/refs":
		return r.Method == http.MethodGet && slices.Equal(r.URL.Query()["service"], []string{"git-upload-pack"})
	case "/git-upload-pack":
		return r.Method == http.MethodPost
	default:
		return false
	}
}

// fetchOnlyArgs are the switches a fetch-only repository is served under; see
// ServeBackend. They are given on the command line so that what they say does
// not depend on the repository's own configuration.
func fetchOnlyArgs() []string {
	return []string{
		"-c", "http.receivepack=false",
		"-c", "http.uploadpack=true",
		"-c", "http.getanyfile=false",
		"-c", "uploadpack.allowAnySHA1InWant=false",
		"-c", "uploadpack.allowReachableSHA1InWant=false",
		"-c", "uploadpack.allowTipSHA1InWant=false",
		"-c", "uploadpack.allowRefInWant=false",
	}
}
