package ahphost

import (
	"encoding/json"
	"strconv"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const watchScheme = "ahp-resource-watch:/"

// watch is one createResourceWatch: the workspace's watch of a file or a
// directory, which lives as long as someone subscribes to it.
type watch struct {
	uri   string
	state ahptypes.ResourceWatchState
	subs  map[*conn]struct{}
	stop  func()
}

func (h *Host) createResourceWatch(c *conn, p ahptypes.CreateResourceWatchParams) (any, error) {
	root, err := localPath(p.Uri)
	if err != nil {
		return nil, err
	}
	w := &watch{
		state: ahptypes.ResourceWatchState{Root: p.Uri, Recursive: p.Recursive != nil && *p.Recursive, Excludes: p.Excludes, Includes: p.Includes},
		subs:  map[*conn]struct{}{},
	}
	h.mu.Lock()
	h.watchSeq++
	w.uri = watchScheme + strconv.FormatInt(h.watchSeq, 10)
	h.mu.Unlock()
	stop, err := h.cfg.Workspace.Watch(root, w.state.Recursive, globs(p.Excludes), globs(p.Includes), func(changes []ahptypes.ResourceChange) {
		raw, err := json.Marshal(struct {
			Items []ahptypes.ResourceChange `json:"items"`
		}{changes})
		if err != nil {
			h.cfg.Logf("ahp: watch %s: %v", root, err)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.watches[w.uri] == w {
			h.dispatch(w.uri, ahptypes.StateAction{Value: &ahptypes.ResourceWatchChangedAction{Type: ahptypes.ActionTypeResourceWatchChanged, Changes: raw}}, nil)
		}
	})
	if err != nil {
		return nil, fsError(err)
	}
	w.stop = stop
	h.mu.Lock()
	defer h.mu.Unlock()
	// The watch is the creator's until it subscribes; one it never
	// subscribes to goes with it.
	w.subs[c] = struct{}{}
	h.watches[w.uri] = w
	return ahptypes.CreateResourceWatchResult{Channel: w.uri}, nil
}

// globs reads a watch's `{ items }` pattern list.
func globs(raw *json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var wrapped struct {
		Items []string `json:"items"`
	}
	if json.Unmarshal(*raw, &wrapped) == nil {
		return wrapped.Items
	}
	var items []string
	_ = json.Unmarshal(*raw, &items)
	return items
}

// releaseWatchLocked stops a watch nobody subscribes to any more, as the
// spec has it: unsubscribing is the only way a client lets one go. h.mu must
// be held.
func (h *Host) releaseWatchLocked(w *watch) {
	if len(w.subs) > 0 {
		return
	}
	delete(h.watches, w.uri)
	w.stop()
}
