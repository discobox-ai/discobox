package ahphost

import (
	"context"
	"slices"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// listTimeout bounds a request that is done when it returns: the harnesses,
// one discobox's execs.
const listTimeout = 30 * time.Second

// harnessRefresh is how often the primary's harnesses are read again: they
// change when someone configures one, which is rare.
const harnessRefresh = time.Minute

// createdGrace is how long a discobox this host just created is kept though
// a listing taken before it existed does not have it.
const createdGrace = time.Minute

// Run lists the discoboxes every Poll until ctx ends, and keeps every session,
// chat and screen in step with what it finds.
func (h *Host) Run(ctx context.Context) error {
	defer h.stop()
	ticker := time.NewTicker(h.cfg.Poll)
	defer ticker.Stop()
	var harnessesAt time.Time
	for {
		if time.Since(harnessesAt) >= harnessRefresh {
			if h.loadHarnesses(ctx) {
				harnessesAt = time.Now()
			}
		}
		h.poll(ctx)
		h.refreshWatchedChangesets()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-h.kicked:
		}
	}
}

// kick asks for a poll now: something was just done that the next listing
// will show.
func (h *Host) kick() {
	select {
	case h.kicked <- struct{}{}:
	default:
	}
}

func (h *Host) loadHarnesses(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	harnesses, err := h.cfg.Boxes.Harnesses(ctx)
	if err != nil {
		h.cfg.Logf("ahp: list harnesses: %v", err)
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.harnesses = map[string]Harness{}
	for _, harness := range harnesses {
		if harness.Slug != "" {
			h.harnesses[harness.Slug] = harness
		}
	}
	h.refreshAgentsLocked()
	return true
}

// poll lists every server under Run's own context. A listing of several
// servers answers with what has come back by its patience and leaves each
// slower request running to be drawn by the next poll (the CLI's
// pollEveryServer), so a context that ended with this poll would cancel every
// request that outlives it, and a server slower than the patience would never
// be listed. The data source bounds each request itself.
func (h *Host) poll(ctx context.Context) {
	listing, err := h.cfg.Boxes.List(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err != nil {
		h.cfg.Logf("ahp: list discoboxes: %v", err)
	} else {
		h.applyListingLocked(listing)
	}
	h.refreshAgentsLocked()
	h.readyOnce.Do(func() { close(h.ready) })
	for _, s := range h.sessions {
		if s.running() && h.watchedLocked(s) {
			go h.refreshExecs(s)
		}
	}
	for _, t := range h.terminals {
		h.ensureAttachedLocked(t)
	}
}

// applyListingLocked brings the sessions in step with a listing. A discobox
// missing from it is gone only when its server answered: one that did not is
// still there as far as anyone knows.
func (h *Host) applyListingLocked(listing Listing) {
	seen := map[string]bool{}
	for _, box := range listing.Boxes {
		seen[box.ID] = true
		uri, ok := h.boxSessions[box.ID]
		if !ok {
			uri = sessionScheme + box.ID
		}
		s := h.sessions[uri]
		if s == nil {
			s = h.newSessionLocked(uri, &box)
			h.addSessionLocked(s)
			continue
		}
		prev := s.box
		s.box = box
		if box.State != prev.State {
			s.starting = false
		}
		// Something new from the agent is something the client has not read.
		if !box.Agent.ReportedAt.Equal(prev.Agent.ReportedAt) && box.Agent.State != prev.Agent.State {
			s.read = false
		}
		h.refreshSessionStateLocked(s)
	}
	quiet := listing.Quiet
	for _, s := range h.sessions {
		if !s.hasBox || seen[s.box.ID] || slices.Contains(quiet, s.box.Server) {
			continue
		}
		if time.Since(s.boxCreated) < createdGrace {
			continue
		}
		h.removeSessionLocked(s)
	}
	h.activeSessionsLocked()
}

// watchedLocked is whether anyone has the session, or one of its chats or
// terminals, open.
func (h *Host) watchedLocked(s *session) bool {
	if len(s.subs) > 0 {
		return true
	}
	for _, c := range h.chats {
		if c.s == s && len(c.subs) > 0 {
			return true
		}
	}
	for _, t := range h.terminals {
		// A shell is attached whether or not anyone watches it, and its exec
		// has to be listed for the host to know when it is gone.
		if t.s == s && (len(t.subs) > 0 || (t.shell() && !t.exited())) {
			return true
		}
	}
	return false
}

// refreshExecs lists a running discobox's execs: which is its primary
// terminal, and which other harness terminals it has, each a chat.
func (h *Host) refreshExecs(s *session) {
	h.mu.Lock()
	if s.listingExecs || !s.running() {
		h.mu.Unlock()
		return
	}
	s.listingExecs = true
	id := s.box.ID
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(h.ctx, listTimeout)
	execs, err := h.cfg.Boxes.Execs(ctx, id)
	cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	s.listingExecs = false
	if err != nil || h.sessions[s.uri] != s {
		return
	}
	// A chat is found by its exec, not by its URI: one a client created
	// carries the URI the client chose.
	byExec := map[string]*chat{}
	for _, ch := range h.chats {
		if ch.s == s && !ch.primary {
			byExec[ch.execID] = ch
		}
	}
	// running is every exec the discobox still runs, whatever its kind.
	running := map[string]bool{}
	for _, exec := range execs {
		if exec.Live {
			running[exec.ID] = true
		}
	}
	live := map[string]bool{}
	for _, exec := range execs {
		if exec.Harness == "" || exec.Tool != "" || exec.Service != "" {
			continue
		}
		if exec.Primary {
			s.primary.execID = exec.ID
			continue
		}
		if !exec.Live {
			continue
		}
		live[exec.ID] = true
		uri := chatScheme + s.key + "/" + exec.ID
		if byExec[exec.ID] != nil || h.chats[uri] != nil {
			continue
		}
		ch := h.newChatLocked(s, uri, false, exec.ID)
		h.dispatch(s.uri, ahptypes.StateAction{Value: &ahptypes.SessionChatAddedAction{Type: ahptypes.ActionTypeSessionChatAdded, Summary: ch.summary()}}, nil)
	}
	for _, ch := range h.chats {
		if ch.s == s && !ch.primary && !live[ch.execID] && time.Since(ch.created) >= createdGrace {
			h.removeChatLocked(ch)
		}
	}
	h.publishSummaryLocked(s, h.summaryLocked(s))
	for _, t := range h.terminals {
		if t.s != s {
			continue
		}
		// An exec the running discobox no longer runs — lost with a restart,
		// failed, gone — is never attached again; its terminal is exited
		// rather than retried for as long as the host runs.
		if execID := t.exec(); execID != "" && !t.exited() && !running[execID] && time.Since(t.created) >= createdGrace {
			h.closeStreamLocked(t)
			h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalExitedAction{Type: ahptypes.ActionTypeTerminalExited}}, nil)
			if t.shell() {
				h.terminalsChangedLocked()
			}
			continue
		}
		h.ensureAttachedLocked(t)
	}
}
