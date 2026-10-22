package ahphost

import (
	"context"
	"net/url"
	"path"
	"reflect"
	"strings"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// A host in a discobox offers its working tree's changes as two changesets
// (ADR 26-10-22-853 §1), read from its Workspace: the branch — everything the
// discobox has changed since it was cut (Box.DiffBase) — and what is not yet
// committed. Each is recomputed every poll while someone watches it.

const changesetScheme = "ahp-changeset:/"

// baseContentScheme is a file as a revision has it, which is what a
// changeset's before side reads (resourceRead):
// `discobox-git:///<path>?rev=<commit>`.
const baseContentScheme = "discobox-git"

const (
	changeKindBranch      = "branch"
	changeKindUncommitted = "uncommitted"
)

type changeset struct {
	uri   string
	s     *session
	kind  string
	state ahptypes.ChangesetState
	subs  map[*conn]struct{}
	// computing is a computation in flight, which a poll does not start a
	// second of.
	computing bool
}

// base is the revision the changeset measures from.
func (cs *changeset) base() string {
	if cs.kind == changeKindBranch {
		return cs.s.box.DiffBase
	}
	return "HEAD"
}

// changesetCatalogLocked is the changesets a session offers: none from the
// CLI, which has no working tree to read, nor for a discobox cut from a
// repository URL with no folder. h.mu must be held.
func (h *Host) changesetCatalogLocked(s *session) []ahptypes.Changeset {
	if h.cfg.Workspace == nil || !s.hasBox || s.box.Source == "" || s.box.SourceRemote {
		return nil
	}
	var out []ahptypes.Changeset
	if s.box.DiffBase != "" {
		out = append(out, ahptypes.Changeset{
			Label:       "Branch Changes",
			UriTemplate: changesetScheme + s.key + "/" + changeKindBranch,
			Description: ptr("Everything changed since the discobox was cut"),
			ChangeKind:  changeKindBranch,
		})
	}
	return append(out, ahptypes.Changeset{
		Label:       "Uncommitted Changes",
		UriTemplate: changesetScheme + s.key + "/" + changeKindUncommitted,
		ChangeKind:  changeKindUncommitted,
	})
}

// refreshChangesetsLocked publishes the session's catalog when it has
// changed, keeping a channel for each entry. h.mu must be held.
func (h *Host) refreshChangesetsLocked(s *session) {
	catalog := h.changesetCatalogLocked(s)
	for _, entry := range catalog {
		if h.changesets[entry.UriTemplate] == nil {
			h.changesets[entry.UriTemplate] = &changeset{uri: entry.UriTemplate, s: s, kind: entry.ChangeKind, subs: map[*conn]struct{}{}, state: ahptypes.ChangesetState{Files: []ahptypes.ChangesetFile{}}}
		}
	}
	if reflect.DeepEqual(catalog, s.state.Changesets) {
		return
	}
	h.dispatch(s.uri, ahptypes.StateAction{Value: &ahptypes.SessionChangesetsChangedAction{Type: ahptypes.ActionTypeSessionChangesetsChanged, Changesets: catalog}}, nil)
}

// refreshWatchedChangesets recomputes every changeset someone subscribes to.
func (h *Host) refreshWatchedChangesets() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cs := range h.changesets {
		if len(cs.subs) > 0 {
			h.computeChangesetLocked(cs)
		}
	}
}

// computeChangesetLocked recomputes a changeset in the background and
// publishes what changed. h.mu must be held.
func (h *Host) computeChangesetLocked(cs *changeset) {
	if cs.computing {
		return
	}
	cs.computing = true
	if cs.state.Status == "" {
		// Its first subscriber is being sent the snapshot this is in.
		cs.state.Status = ahptypes.ChangesetStatusComputing
	}
	dir, base := cs.s.box.Source, cs.base()
	go func() {
		ctx, cancel := context.WithTimeout(h.ctx, listTimeout)
		defer cancel()
		changes, err := h.cfg.Workspace.Changes(ctx, dir, base)
		files := changesetFiles(changes, base)
		h.mu.Lock()
		defer h.mu.Unlock()
		cs.computing = false
		if err != nil {
			if cs.state.Status != ahptypes.ChangesetStatusError {
				h.dispatch(cs.uri, ahptypes.StateAction{Value: &ahptypes.ChangesetStatusChangedAction{Type: ahptypes.ActionTypeChangesetStatusChanged, Status: ahptypes.ChangesetStatusError, Error: &ahptypes.ErrorInfo{ErrorType: "git", Message: err.Error()}}}, nil)
			}
			return
		}
		if !reflect.DeepEqual(files, cs.state.Files) {
			h.dispatch(cs.uri, ahptypes.StateAction{Value: &ahptypes.ChangesetContentChangedAction{Type: ahptypes.ActionTypeChangesetContentChanged, Files: files}}, nil)
		}
		if cs.state.Status != ahptypes.ChangesetStatusReady {
			h.dispatch(cs.uri, ahptypes.StateAction{Value: &ahptypes.ChangesetStatusChangedAction{Type: ahptypes.ActionTypeChangesetStatusChanged, Status: ahptypes.ChangesetStatusReady}}, nil)
		}
	}()
}

// changesetFiles is the workspace's changes as a changeset's files: an
// edit's before side reads the file as base has it (baseContentURI), its after
// side the file in the working tree.
func changesetFiles(changes []Changed, base string) []ahptypes.ChangesetFile {
	files := make([]ahptypes.ChangesetFile, 0, len(changes))
	for _, change := range changes {
		uri := fileURI(change.Path)
		edit := ahptypes.FileEdit{}
		if change.Before {
			edit.Before = &ahptypes.FileEditSide{Uri: uri, Content: ahptypes.ContentRef{Uri: baseContentURI(change.Path, base)}}
		}
		if change.After {
			edit.After = &ahptypes.FileEditSide{Uri: uri, Content: ahptypes.ContentRef{Uri: uri}}
		}
		if change.Added != nil || change.Removed != nil {
			edit.Diff = &ahptypes.FileEditDiffStats{Added: change.Added, Removed: change.Removed}
		}
		files = append(files, ahptypes.ChangesetFile{Id: uri, Edit: edit})
	}
	return files
}

func baseContentURI(path, rev string) string {
	return (&url.URL{Scheme: baseContentScheme, Path: path, RawQuery: url.Values{"rev": {rev}}.Encode()}).String()
}

// readBase reads a file as a revision has it (baseContentURI).
func (h *Host) readBase(ctx context.Context, uri string) ([]byte, error) {
	u, err := url.Parse(uri)
	rev := ""
	if err == nil {
		rev = u.Query().Get("rev")
	}
	if err != nil || !path.IsAbs(u.Path) || rev == "" || strings.HasPrefix(rev, "-") {
		return nil, errorf(ahptypes.ErrorCodeInvalidParams, "%s names no file at a revision", uri)
	}
	return h.cfg.Workspace.ReadRevision(ctx, path.Clean(u.Path), rev)
}
