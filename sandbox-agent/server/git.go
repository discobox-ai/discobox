package server

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	"github.com/discobox-ai/discobox/gitbackend"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// serveGitRepositoryHTTP serves one of this sandbox's own source checkouts
// over git's smart HTTP protocol: `discobox apply` fetches from it, and a
// client clones, fetches and pushes through it (ADR 0126 §4). The pool
// forwards its worktree route here; the token was checked against this
// sandbox and against sandbox:read or sandbox:write (requiredRequestScope)
// before this runs.
//
// A repository is named by its source's slug and resolves only among this
// sandbox's sources, so no name reaches another sandbox's repository or any
// other directory in this one. The backend runs as the sandbox's user, who
// owns the checkout, and a push updates the checked-out branch in place
// (updateInstead), the way a push into the worktree always has.
func (h *handler) serveGitRepositoryHTTP(w http.ResponseWriter, r *http.Request) {
	slug, suffix, ok := gitbackend.ParseRepositoryPath(chi.URLParam(r, "*"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	root, ok := h.sourceCheckout(slug)
	if !ok {
		http.Error(w, "repository not found: "+slug, http.StatusNotFound)
		return
	}
	// A push-delivered source has no checkout until its delivery lands
	// (ADR 0001): not found until then, as it always was.
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "repository not found: "+slug, http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	attr, err := execs.AgentSysProcAttr(h.execUser)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	what := "git fetch from " + slug
	if gitbackend.IsReceivePack(r) {
		what = "git push to " + slug
	}
	release := h.autostop.Hold(what)
	defer release()
	gitbackend.Serve(w, r, gitbackend.Backend{
		Root: root,
		Config: []string{
			"-c", "http.receivepack=true",
			"-c", "receive.denyCurrentBranch=updateInstead",
		},
		RemoteUser:  "sandbox-agent",
		SysProcAttr: attr,
	}, suffix)
}

// sourceCheckout is where the source named slug is checked out in this
// sandbox.
func (h *handler) sourceCheckout(slug string) (string, bool) {
	for _, source := range h.sources {
		if source.Slug == slug && source.Target != "" {
			return source.Target, true
		}
	}
	return "", false
}
