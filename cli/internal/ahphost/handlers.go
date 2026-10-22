package ahphost

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"sort"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// protocolVersion is the one version this host speaks, the SDK's: VS Code
// offers it and does not offer anything newer.
const protocolVersion = "1.0.0"

// readyWait bounds how long initialize and listSessions wait for the first
// listing before answering with what there is.
const readyWait = 15 * time.Second

// opTimeout bounds a request that has to ask a server something.
const opTimeout = 2 * time.Minute

// handleRequest answers one request. Those that only read the host's state
// are answered in order on the client's reader; those that ask a server are
// answered from a goroutine, so a slow server holds up nothing else.
func (h *Host) handleRequest(c *conn, id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "initialize":
		h.initialize(c, id, params)
	case "ping":
		c.reply(id, nil, nil)
	case "reconnect":
		// Nothing is kept for a client that has gone. NotFound is what makes
		// VS Code initialize afresh rather than give up.
		c.reply(id, nil, errorf(ahptypes.ErrorCodeNotFound, "this host does not replay; initialize again"))
	case "subscribe":
		var p ahptypes.SubscribeParams
		if err := decode(params, &p); err != nil {
			c.reply(id, nil, err)
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		snap, err := h.subscribeLocked(c, p.Channel)
		if err != nil {
			c.reply(id, nil, err)
			return
		}
		c.reply(id, ahptypes.SubscribeResult{Snapshot: snap}, nil)
	case "listSessions":
		ctx, cancel := context.WithTimeout(c.ctx, readyWait)
		h.waitReady(ctx)
		cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		c.reply(id, ahptypes.ListSessionsResult{Items: h.listLocked()}, nil)
	case "createSession":
		var p ahptypes.CreateSessionParams
		if err := decode(params, &p); err != nil {
			c.reply(id, nil, err)
			return
		}
		c.reply(id, nil, h.createSession(p))
	case "disposeSession", "createChat", "disposeChat", "createTerminal", "disposeTerminal":
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, opTimeout)
			defer cancel()
			c.reply(id, nil, h.handleOperation(ctx, method, params))
		}()
	default:
		c.reply(id, nil, errorf(ahptypes.ErrorCodeMethodNotFound, "%s is not served by this host", method))
	}
}

func (h *Host) handleOperation(ctx context.Context, method string, params json.RawMessage) error {
	switch method {
	case "disposeSession":
		var p ahptypes.DisposeSessionParams
		if err := decode(params, &p); err != nil {
			return err
		}
		return h.disposeSession(ctx, p.Channel)
	case "createChat":
		var p ahptypes.CreateChatParams
		if err := decode(params, &p); err != nil {
			return err
		}
		return h.createChat(ctx, p)
	case "disposeChat":
		var p ahptypes.DisposeChatParams
		if err := decode(params, &p); err != nil {
			return err
		}
		return h.disposeChat(ctx, p.Channel)
	case "createTerminal":
		var p ahptypes.CreateTerminalParams
		if err := decode(params, &p); err != nil {
			return err
		}
		return h.createTerminal(ctx, p)
	default:
		var p ahptypes.DisposeTerminalParams
		if err := decode(params, &p); err != nil {
			return err
		}
		return h.disposeTerminal(ctx, p.Channel)
	}
}

func decode(params json.RawMessage, v any) error {
	if err := json.Unmarshal(params, v); err != nil {
		return errorf(ahptypes.ErrorCodeInvalidParams, "%v", err)
	}
	return nil
}

// handleNotification is a client's notification.
func (h *Host) handleNotification(c *conn, method string, params json.RawMessage) {
	switch method {
	case "unsubscribe":
		var p ahptypes.UnsubscribeParams
		if decode(params, &p) != nil {
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if subs := h.subscribers(p.Channel); subs != nil {
			delete(subs, c)
		}
		if t := h.terminals[p.Channel]; t != nil {
			h.releaseLocked(t)
		}
	case "dispatchAction":
		var p ahptypes.DispatchActionParams
		if err := decode(params, &p); err != nil {
			h.cfg.Logf("ahp: %v", err)
			return
		}
		h.mu.Lock()
		origin := &ahptypes.ActionOrigin{ClientId: c.clientID, ClientSeq: p.ClientSeq}
		s, ch, t := h.sessions[p.Channel], h.chats[p.Channel], h.terminals[p.Channel]
		switch {
		case ch != nil:
			h.handleChatAction(ch, p.Action, origin)
			h.mu.Unlock()
		case s != nil:
			h.mu.Unlock()
			h.handleSessionAction(s, p.Action, origin)
		case t != nil:
			h.mu.Unlock()
			h.handleTerminalAction(t, p.Action, origin)
		default:
			// The root's only client action is root/configChanged, which
			// configures nothing here; an action on a channel that does not
			// exist is ignored, as the spec says.
			h.mu.Unlock()
		}
	}
}

func (h *Host) initialize(c *conn, id json.RawMessage, params json.RawMessage) {
	var p ahptypes.InitializeParams
	if err := decode(params, &p); err != nil {
		c.reply(id, nil, err)
		return
	}
	if !slices.Contains(p.ProtocolVersions, protocolVersion) {
		c.reply(id, nil, errorf(ahptypes.ErrorCodeUnsupportedProtocolVersion, "this host speaks protocol %s only", protocolVersion))
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, readyWait)
	h.waitReady(ctx)
	cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	c.clientID = p.ClientId
	// The root's snapshot is always sent: VS Code reads the agents from it
	// and subscribes to the root no other way.
	channels := append([]string{ahptypes.RootResourceURI}, p.InitialSubscriptions...)
	snapshots := []ahptypes.Snapshot{}
	seen := map[string]bool{}
	for _, channel := range channels {
		if seen[channel] {
			continue
		}
		seen[channel] = true
		if snap, err := h.subscribeLocked(c, channel); err == nil {
			snapshots = append(snapshots, *snap)
		}
	}
	version := h.cfg.Version
	c.reply(id, ahptypes.InitializeResult{
		ProtocolVersion: protocolVersion,
		ServerSeq:       h.seq,
		ServerInfo:      &ahptypes.Implementation{Name: "discobox", Version: &version},
		Snapshots:       snapshots,
	}, nil)
}

// subscribeLocked adds c to a channel's subscribers and says what the channel
// holds. A screen someone now watches is attached, and one already attached
// is repainted, so the newcomer sees the whole screen and not only what
// changes from here.
func (h *Host) subscribeLocked(c *conn, channel string) (*ahptypes.Snapshot, error) {
	snap := &ahptypes.Snapshot{Resource: channel, FromSeq: h.seq}
	switch {
	case channel == ahptypes.RootResourceURI:
		h.rootSubs[c] = struct{}{}
		snap.State.Root = &h.root
	case h.sessions[channel] != nil:
		s := h.sessions[channel]
		s.subs[c] = struct{}{}
		snap.State.Session = &s.state
		go h.refreshExecs(s)
	case h.chats[channel] != nil:
		ch := h.chats[channel]
		ch.subs[c] = struct{}{}
		snap.State.Chat = &ch.state
	case h.terminals[channel] != nil:
		t := h.terminals[channel]
		t.subs[c] = struct{}{}
		snap.State.Terminal = &t.state
		if stream := t.stream; stream != nil {
			go func() { _ = stream.Repaint() }()
		}
		h.ensureAttachedLocked(t)
	default:
		return nil, errorf(ahptypes.ErrorCodeNotFound, "no channel %s", channel)
	}
	return snap, nil
}

// listLocked is the session list, most recently changed first.
func (h *Host) listLocked() []ahptypes.SessionSummary {
	items := make([]ahptypes.SessionSummary, 0, len(h.sessions))
	for _, s := range h.sessions {
		items = append(items, h.summaryLocked(s))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ModifiedAt != items[j].ModifiedAt {
			return items[i].ModifiedAt > items[j].ModifiedAt
		}
		return items[i].Resource < items[j].Resource
	})
	return items
}

// handleSessionAction is a client's action on a session; h.mu is not held.
// Archiving and renaming are the discobox's, and are echoed once its server
// has done them, or rejected.
func (h *Host) handleSessionAction(s *session, act ahptypes.StateAction, origin *ahptypes.ActionOrigin) {
	var do func(ctx context.Context, id string) error
	switch a := act.Value.(type) {
	case *ahptypes.SessionIsArchivedChangedAction:
		verb := tui.VerbUnarchive
		if a.IsArchived {
			verb = tui.VerbArchive
		}
		do = func(ctx context.Context, id string) error { return h.cfg.Boxes.Do(ctx, verb, id) }
	case *ahptypes.SessionTitleChangedAction:
		do = func(ctx context.Context, id string) error { return h.cfg.Boxes.Rename(ctx, id, a.Title) }
	case *ahptypes.SessionIsReadChangedAction:
		h.mu.Lock()
		defer h.mu.Unlock()
		s.read = a.IsRead
		h.dispatch(s.uri, act, origin)
		h.publishSummaryLocked(s, h.summaryLocked(s))
		return
	default:
		h.mu.Lock()
		defer h.mu.Unlock()
		h.dispatch(s.uri, act, origin)
		return
	}
	h.mu.Lock()
	id := s.boxID()
	h.mu.Unlock()
	go func() {
		err := errorf(ahptypes.ErrorCodeNotFound, "this session has no discobox yet")
		if id != "" {
			ctx, cancel := context.WithTimeout(h.ctx, opTimeout)
			if doErr := do(ctx, id); doErr != nil {
				err = errorf(ahptypes.ErrorCodeInternalError, "%v", doErr)
			} else {
				err = nil
			}
			cancel()
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if err != nil {
			h.reject(s.uri, act, origin, err.Error())
			return
		}
		h.dispatch(s.uri, act, origin)
		h.kick()
	}()
}

// createSession makes a session with no discobox yet: its first message is
// the prompt the discobox is created with (deliver). It is cut from the folder
// the client named, when that is a folder on this machine, and from the
// directory the host runs in otherwise.
func (h *Host) createSession(p ahptypes.CreateSessionParams) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[p.Channel] != nil {
		return errorf(ahptypes.ErrorCodeSessionAlreadyExists, "session %s exists", p.Channel)
	}
	harness := ""
	if p.Provider != nil {
		harness = *p.Provider
		if !slices.ContainsFunc(h.root.Agents, func(a ahptypes.AgentInfo) bool { return a.Provider == harness }) {
			return errorf(ahptypes.ErrorCodeProviderNotFound, "no harness %s", harness)
		}
	}
	source := h.cfg.Source
	for _, dir := range p.WorkingDirectories {
		if u, err := url.Parse(dir); err == nil && u.Scheme == "file" && u.Host == "" && u.Path != "" {
			source = u.Path
			break
		}
	}
	s := h.newSessionLocked(p.Channel, nil)
	s.harness, s.source = harness, source
	h.addSessionLocked(s)
	return nil
}

// disposeSession archives the discobox (ADR 26-10-17-354 §2): it holds work
// nobody has applied, and VS Code disposes from a gesture in its session
// list. A session with no discobox yet is only forgotten.
func (h *Host) disposeSession(ctx context.Context, uri string) error {
	h.mu.Lock()
	s := h.sessions[uri]
	if s == nil {
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeSessionNotFound, "no session %s", uri)
	}
	if !s.hasBox {
		if s.creating {
			h.mu.Unlock()
			return errorf(ahptypes.ErrorCodeTurnInProgress, "its discobox is being created")
		}
		h.removeSessionLocked(s)
		h.mu.Unlock()
		return nil
	}
	id := s.box.ID
	h.mu.Unlock()
	if err := h.cfg.Boxes.Do(ctx, tui.VerbArchive, id); err != nil {
		return err
	}
	h.kick()
	return nil
}

// createChat starts another session of the discobox's harness.
func (h *Host) createChat(ctx context.Context, p ahptypes.CreateChatParams) error {
	h.mu.Lock()
	s := h.sessions[p.Channel]
	switch {
	case s == nil:
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeSessionNotFound, "no session %s", p.Channel)
	case h.chats[p.Chat] != nil:
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeAlreadyExists, "chat %s exists", p.Chat)
	case !s.running():
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodePermissionDenied, "the discobox is not running")
	}
	id := s.box.ID
	h.mu.Unlock()
	openCtx, opened, release := h.openContext(ctx)
	exec, stream, err := h.cfg.Boxes.NewTerminal(openCtx, id, defaultCols, defaultRows)
	if err != nil {
		opened(false)
		return err
	}
	if !opened(true) {
		go h.abandon(stream, release, id, exec.ID)
		return errorf(ahptypes.ErrorCodeInternalError, "the terminal did not open in time")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// The session may have gone, or another chat taken the URI, while the
	// terminal was being made.
	if h.sessions[s.uri] != s || h.chats[p.Chat] != nil {
		go h.abandon(stream, release, id, exec.ID)
		return errorf(ahptypes.ErrorCodeAlreadyExists, "chat %s could not be added", p.Chat)
	}
	// A listing that landed while the terminal was being made may already
	// have made a chat of its exec; this one, at the URI the client chose,
	// takes its place.
	for _, other := range h.chats {
		if other.s == s && !other.primary && other.execID == exec.ID {
			h.removeChatLocked(other)
		}
	}
	ch := h.newChatLocked(s, p.Chat, false, exec.ID)
	ch.created = time.Now()
	h.adoptLocked(ch.screen, stream, release)
	h.dispatch(s.uri, ahptypes.StateAction{Value: &ahptypes.SessionChatAddedAction{Type: ahptypes.ActionTypeSessionChatAdded, Summary: ch.summary()}}, nil)
	h.publishSummaryLocked(s, h.summaryLocked(s))
	if p.InitialMessage != nil {
		h.endTurnLocked(ch)
		turnID := h.startTurnLocked(ch, *p.InitialMessage, nil, nil)
		h.syncChatSummaryLocked(ch)
		go h.deliver(ch, turnID, *p.InitialMessage)
	}
	return nil
}

// abandon lets go of an exec a request made and could not hand over — its
// open lost a race with its deadline, or its session or URI was gone by the
// time it opened — ending it rather than leaving it running in the discobox
// where nothing shows it.
func (h *Host) abandon(stream tui.Terminal, release func(), sandboxID, execID string) {
	_ = stream.Close()
	release()
	ctx, cancel := context.WithTimeout(h.ctx, opTimeout)
	defer cancel()
	if err := h.cfg.Boxes.EndExec(ctx, sandboxID, execID); err != nil {
		h.cfg.Logf("ahp: end %s/%s: %v", sandboxID, execID, err)
	}
}

// disposeChat ends a harness terminal. The primary is the discobox's own and
// is not a client's to end.
func (h *Host) disposeChat(ctx context.Context, uri string) error {
	h.mu.Lock()
	ch := h.chats[uri]
	switch {
	case ch == nil:
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeNotFound, "no chat %s", uri)
	case ch.primary:
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodePermissionDenied, "the primary terminal is the discobox's own")
	}
	id, execID := ch.s.box.ID, ch.execID
	h.mu.Unlock()
	if err := h.cfg.Boxes.EndExec(ctx, id, execID); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeChatLocked(ch)
	return nil
}

// removeChatLocked forgets a chat and its screens.
func (h *Host) removeChatLocked(ch *chat) {
	if h.chats[ch.uri] != ch {
		return
	}
	delete(h.chats, ch.uri)
	for uri, t := range h.terminals {
		if t.chat == ch {
			h.closeStreamLocked(t)
			delete(h.terminals, uri)
		}
	}
	h.dispatch(ch.s.uri, ahptypes.StateAction{Value: &ahptypes.SessionChatRemovedAction{Type: ahptypes.ActionTypeSessionChatRemoved, Chat: ch.uri}}, nil)
	h.publishSummaryLocked(ch.s, h.summaryLocked(ch.s))
}

// createTerminal opens a shell in the discobox the working directory names —
// which is how VS Code asks for one for the session it shows (sessions.go,
// workingDirectory). A discobox that is not running is not started for it:
// the terminal says so and is exited.
func (h *Host) createTerminal(ctx context.Context, p ahptypes.CreateTerminalParams) error {
	h.mu.Lock()
	if h.terminals[p.Channel] != nil {
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeAlreadyExists, "terminal %s exists", p.Channel)
	}
	var s *session
	if p.Cwd != nil {
		s = h.boxFromCwd(*p.Cwd)
	}
	if s == nil {
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeInvalidParams, "the working directory names no discobox")
	}
	title := "Shell"
	if p.Name != nil && *p.Name != "" {
		title = *p.Name
	}
	t := &terminal{uri: p.Channel, s: s, subs: map[*conn]struct{}{}, cols: defaultCols, rows: defaultRows, created: time.Now(), state: newTerminalState(title, p.Claim)}
	if p.Cols != nil && p.Rows != nil {
		t.cols, t.rows = int(*p.Cols), int(*p.Rows)
	}
	if !s.running() {
		h.terminals[t.uri] = t
		h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalDataAction{
			Type: ahptypes.ActionTypeTerminalData, Data: "\x1b[2m[discobox " + s.box.Name + " is " + string(s.box.State) + "]\x1b[0m\r\n",
		}}, nil)
		h.dispatch(t.uri, ahptypes.StateAction{Value: &ahptypes.TerminalExitedAction{Type: ahptypes.ActionTypeTerminalExited}}, nil)
		h.terminalsChangedLocked()
		h.mu.Unlock()
		return nil
	}
	id, cols, rows := s.box.ID, t.cols, t.rows
	h.mu.Unlock()
	openCtx, opened, release := h.openContext(ctx)
	exec, stream, err := h.cfg.Boxes.NewShell(openCtx, id, cols, rows)
	if err != nil {
		opened(false)
		return err
	}
	if !opened(true) {
		go h.abandon(stream, release, id, exec.ID)
		return errorf(ahptypes.ErrorCodeInternalError, "the terminal did not open in time")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.terminals[t.uri] != nil {
		go h.abandon(stream, release, id, exec.ID)
		return errorf(ahptypes.ErrorCodeAlreadyExists, "terminal %s exists", p.Channel)
	}
	t.execID = exec.ID
	h.terminals[t.uri] = t
	h.adoptLocked(t, stream, release)
	h.terminalsChangedLocked()
	return nil
}

// disposeTerminal ends a shell. A screen is its chat's, and goes with it.
func (h *Host) disposeTerminal(ctx context.Context, uri string) error {
	h.mu.Lock()
	t := h.terminals[uri]
	switch {
	case t == nil:
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodeNotFound, "no terminal %s", uri)
	case !t.shell():
		h.mu.Unlock()
		return errorf(ahptypes.ErrorCodePermissionDenied, "a chat's terminal is disposed with its chat")
	}
	h.closeStreamLocked(t)
	delete(h.terminals, uri)
	h.terminalsChangedLocked()
	id, execID, exited := t.s.boxID(), t.execID, t.exited()
	h.mu.Unlock()
	if execID == "" || exited || id == "" {
		return nil
	}
	return h.cfg.Boxes.EndExec(ctx, id, execID)
}
