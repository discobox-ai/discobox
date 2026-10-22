package ahphost

import (
	"net/url"
	"reflect"
	"sort"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
)

// session is one discobox as an AHP session. A session a client created has
// no discobox until its first message makes one (deliver), since that message
// is the prompt the discobox is created with.
type session struct {
	uri string
	key string

	box    Box
	hasBox bool

	// harness, source and createdAt are a created session's, from
	// createSession, until it has a discobox to read them from.
	harness   string
	source    string
	createdAt time.Time
	// boxCreated is when this host created the session's discobox, which a
	// listing taken just before does not have yet.
	boxCreated time.Time
	// creating is a create in flight, which a second message must not start
	// again.
	creating bool
	// starting is a start this host asked for, so typing into a stopped
	// discobox asks once.
	starting bool

	// listed is whether the session is in the session list. A session a
	// client created is not until its first message makes its discobox: VS
	// Code creates one for every new-session draft it opens, and listing
	// them would fill the list with sessions nobody sent anything to.
	listed bool
	// owner is the client that created a draft. Only its going forgets the
	// draft: another client's disconnect says nothing about it.
	owner *conn

	// read is the client's IsRead flag. Discobox has no such state; the host
	// keeps it so a listing does not reset it, and clears it when the agent
	// reports something new.
	read bool

	state   ahptypes.SessionState
	summary ahptypes.SessionSummary
	subs    map[*conn]struct{}
	primary *chat
	// listingExecs is an exec listing in flight, so a poll does not start a
	// second one behind a slow server.
	listingExecs bool
}

func (s *session) boxID() string {
	if !s.hasBox {
		return ""
	}
	return s.box.ID
}

func (s *session) running() bool { return s.hasBox && s.box.State == BoxRunning }

func (s *session) provider() string {
	if s.hasBox && s.box.Harness != "" {
		return s.box.Harness
	}
	return s.harness
}

func (s *session) title() string {
	if s.hasBox {
		return s.box.Name
	}
	return "New discobox"
}

// newSessionLocked makes the session for uri, with its primary chat: a
// discobox's when box is given, and a created session's otherwise. h.mu must
// be held.
func (h *Host) newSessionLocked(uri string, box *Box) *session {
	s := &session{uri: uri, key: sessionKey(uri), subs: map[*conn]struct{}{}, createdAt: time.Now()}
	if box != nil {
		s.box, s.hasBox = *box, true
		h.boxSessions[box.ID] = uri
	}
	h.sessions[uri] = s
	s.primary = h.newChatLocked(s, chatScheme+s.key+"/"+primaryChatKey, true, "")
	s.state = ahptypes.SessionState{
		Lifecycle:     ahptypes.SessionLifecycleReady,
		ActiveClients: []ahptypes.SessionActiveClient{},
		Chats:         []ahptypes.ChatSummary{s.primary.summary()},
		DefaultChat:   ptr(s.primary.uri),
	}
	h.refreshSessionStateLocked(s)
	return s
}

// summaryLocked is the session as a session list shows it, derived from the
// discobox each time (ADR 26-10-17-354 §2). h.mu must be held.
func (h *Host) summaryLocked(s *session) ahptypes.SessionSummary {
	created := s.createdAt
	modified := s.createdAt
	if s.hasBox {
		created = s.box.Created
		modified = s.box.Created
		if s.box.Agent.ReportedAt.After(modified) {
			modified = s.box.Agent.ReportedAt
		}
	}
	sum := ahptypes.SessionSummary{
		Resource:    s.uri,
		Provider:    s.provider(),
		Title:       s.title(),
		Status:      h.statusLocked(s),
		CreatedAt:   timestamp(created),
		ModifiedAt:  timestamp(modified),
		DefaultChat: ptr(s.primary.uri),
	}
	if activity := activity(s); activity != "" {
		sum.Activity = &activity
	}
	if dir := h.workingDirectory(s); dir != "" {
		sum.WorkingDirectories = []ahptypes.URI{dir}
	}
	chatURIs := make([]string, 0, len(h.chats))
	for uri, c := range h.chats {
		if c.s == s {
			chatURIs = append(chatURIs, uri)
		}
	}
	sort.Slice(chatURIs, func(i, j int) bool {
		// The primary first, as the workspace draws it.
		pi, pj := h.chats[chatURIs[i]].primary, h.chats[chatURIs[j]].primary
		if pi != pj {
			return pi
		}
		return chatURIs[i] < chatURIs[j]
	})
	for _, uri := range chatURIs {
		c := h.chats[uri]
		sum.Chats = append(sum.Chats, ahptypes.SessionChatSummary{Resource: uri, Title: c.state.Title, Status: ptr(c.state.Status)})
	}
	return sum
}

// statusLocked maps what the primary terminal's program last reported over
// OSC 7501 onto the session's activity bits (ADR 26-10-17-354 §2), and adds
// the read and archived flags.
func (h *Host) statusLocked(s *session) ahptypes.SessionStatus {
	status := ahptypes.SessionStatusIdle
	if s.hasBox && s.box.State == BoxRunning {
		switch s.box.Agent.State {
		case programstatus.Working:
			status = ahptypes.SessionStatusInProgress
		case programstatus.Blocked:
			status = ahptypes.SessionStatusInputNeeded
		case programstatus.Error:
			status = ahptypes.SessionStatusError
		}
	}
	if s.read {
		status |= ahptypes.SessionStatusIsRead
	}
	if s.hasBox && s.box.State == BoxArchived {
		status |= ahptypes.SessionStatusIsArchived
	}
	return status
}

// activity is what the session list says the discobox is doing: what its
// agent last reported, or why it is not doing anything.
func activity(s *session) string {
	if !s.hasBox {
		if s.creating {
			return "creating"
		}
		return ""
	}
	switch s.box.State {
	case BoxRunning:
	case BoxArchived:
		return ""
	default:
		return string(s.box.State)
	}
	if s.box.Agent.Msg != "" {
		return s.box.Agent.Msg
	}
	return s.box.Agent.Title
}

// workingDirectory is the session's folder: the discobox's working tree, a
// path in the discobox (Box.Workdir), whose files the host serves and where
// VS Code opens a terminal for the session. From the CLI, which serves many
// discoboxes, it names the discobox in a file URI's authority — the path
// alone would not say which discobox's. A host in the discobox serves the
// one, and its folder is the plain local path, which is what Open in Editor
// opens over Remote-SSH.
func (h *Host) workingDirectory(s *session) string {
	if !s.hasBox {
		return ""
	}
	paths := sandboxpath.For(s.box.Platform)
	path := s.box.Workdir
	if path == "" {
		path = paths.WorkingRoot()
	}
	u := url.URL{Scheme: "file", Path: toURIPath(paths, path)}
	if h.cfg.Workspace == nil {
		u.Host = s.box.ID
	}
	return u.String()
}

// boxFromCwd is the session a terminal's working directory names
// (workingDirectory), or nil. In a discobox, every local folder is its own.
func (h *Host) boxFromCwd(cwd string) *session {
	u, err := url.Parse(cwd)
	if err != nil || u.Scheme != "file" {
		return nil
	}
	if h.cfg.Workspace != nil {
		if u.Host != "" {
			return nil
		}
		for _, uri := range h.boxSessions {
			return h.sessions[uri]
		}
		return nil
	}
	if uri, ok := h.boxSessions[u.Host]; ok && u.Host != "" {
		return h.sessions[uri]
	}
	return nil
}

// refreshSessionStateLocked brings the session channel's own copy of the
// summary fields up to date with actions, and publishes any change to the
// session list. h.mu must be held.
func (h *Host) refreshSessionStateLocked(s *session) {
	sum := h.summaryLocked(s)
	if s.state.Title != sum.Title {
		h.dispatch(s.uri, ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{Type: ahptypes.ActionTypeSessionTitleChanged, Title: sum.Title}}, nil)
	}
	if !reflect.DeepEqual(s.state.Activity, sum.Activity) {
		h.dispatch(s.uri, ahptypes.StateAction{Value: &ahptypes.SessionActivityChangedAction{Type: ahptypes.ActionTypeSessionActivityChanged, Activity: sum.Activity}}, nil)
	}
	archived := sum.Status&ahptypes.SessionStatusIsArchived != 0
	if (s.state.Status&ahptypes.SessionStatusIsArchived != 0) != archived {
		h.dispatch(s.uri, ahptypes.StateAction{Value: &ahptypes.SessionIsArchivedChangedAction{Type: ahptypes.ActionTypeSessionIsArchivedChanged, IsArchived: archived}}, nil)
	}
	// The activity bits have no action of their own on the session channel;
	// the snapshot carries them, and the session list's notification below.
	s.state.Provider = sum.Provider
	s.state.Status = sum.Status
	s.state.WorkingDirectories = sum.WorkingDirectories
	h.publishSummaryLocked(s, sum)
	h.refreshChangesetsLocked(s)
}

// publishSummaryLocked tells the root's subscribers what changed in a
// session's summary since they were last told.
func (h *Host) publishSummaryLocked(s *session, sum ahptypes.SessionSummary) {
	prev := s.summary
	s.summary = sum
	if prev.Resource == "" || !s.listed {
		return
	}
	var changes ahptypes.PartialSessionSummary
	changed := false
	if prev.Title != sum.Title {
		changes.Title, changed = &sum.Title, true
	}
	if prev.Status != sum.Status {
		changes.Status, changed = &sum.Status, true
	}
	if !reflect.DeepEqual(prev.Activity, sum.Activity) {
		changes.Activity, changed = sum.Activity, true
		if sum.Activity == nil {
			changes.Activity = ptr("")
		}
	}
	if prev.ModifiedAt != sum.ModifiedAt {
		changes.ModifiedAt, changed = &sum.ModifiedAt, true
	}
	if prev.Provider != sum.Provider {
		changes.Provider, changed = &sum.Provider, true
	}
	if !reflect.DeepEqual(prev.WorkingDirectories, sum.WorkingDirectories) {
		changes.WorkingDirectories, changed = sum.WorkingDirectories, true
	}
	if !reflect.DeepEqual(prev.Chats, sum.Chats) {
		changes.Chats, changed = sum.Chats, true
	}
	if !changed {
		return
	}
	h.notifyRoot("root/sessionSummaryChanged", ahptypes.SessionSummaryChangedParams{
		Channel: ahptypes.RootResourceURI, Session: s.uri, Changes: changes,
	})
}

// addSessionLocked publishes a new session to the root's subscribers.
func (h *Host) addSessionLocked(s *session) {
	s.listed = true
	s.summary = h.summaryLocked(s)
	h.notifyRoot("root/sessionAdded", ahptypes.SessionAddedParams{Channel: ahptypes.RootResourceURI, Summary: s.summary})
	h.activeSessionsLocked()
}

// removeSessionLocked forgets a discobox that is gone, its chats and screens
// with it.
func (h *Host) removeSessionLocked(s *session) {
	delete(h.sessions, s.uri)
	if s.hasBox {
		delete(h.boxSessions, s.box.ID)
	}
	for uri, c := range h.chats {
		if c.s == s {
			delete(h.chats, uri)
		}
	}
	for uri, t := range h.terminals {
		if t.s == s {
			h.closeStreamLocked(t)
			delete(h.terminals, uri)
		}
	}
	if s.listed {
		h.notifyRoot("root/sessionRemoved", ahptypes.SessionRemovedParams{Channel: ahptypes.RootResourceURI, Session: s.uri})
	}
	h.terminalsChangedLocked()
	h.activeSessionsLocked()
}

func (h *Host) activeSessionsLocked() {
	n := int64(0)
	for _, s := range h.sessions {
		if s.hasBox && s.box.State != BoxArchived {
			n++
		}
	}
	if h.root.ActiveSessions != nil && *h.root.ActiveSessions == n {
		return
	}
	h.dispatch(ahptypes.RootResourceURI, ahptypes.StateAction{Value: &ahptypes.RootActiveSessionsChangedAction{
		Type: ahptypes.ActionTypeRootActiveSessionsChanged, ActiveSessions: n,
	}}, nil)
}

// agentsLocked is the root's agent list: a harness the primary offers, and
// any other a listed discobox runs, since a session whose provider is not an
// agent does not open. Each has one model, named for the harness: VS Code
// will not send a message to an agent with none, and the harness picks its
// own.
func (h *Host) agentsLocked() []ahptypes.AgentInfo {
	names := map[string]string{}
	for slug, harness := range h.harnesses {
		names[slug] = harness.Name
	}
	for _, s := range h.sessions {
		if p := s.provider(); p != "" {
			if _, ok := names[p]; !ok {
				names[p] = p
				if s.hasBox && s.box.HarnessName != "" {
					names[p] = s.box.HarnessName
				}
			}
		}
	}
	slugs := make([]string, 0, len(names))
	for slug := range names {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	agents := make([]ahptypes.AgentInfo, 0, len(slugs))
	for _, slug := range slugs {
		name := names[slug]
		if name == "" {
			name = slug
		}
		agents = append(agents, ahptypes.AgentInfo{
			Provider:    slug,
			DisplayName: name,
			Description: name + " in a discobox",
			Models:      []ahptypes.SessionModelInfo{{Id: slug, Provider: slug, Name: name}},
		})
	}
	return agents
}

func (h *Host) refreshAgentsLocked() {
	agents := h.agentsLocked()
	if reflect.DeepEqual(agents, h.root.Agents) {
		return
	}
	h.dispatch(ahptypes.RootResourceURI, ahptypes.StateAction{Value: &ahptypes.RootAgentsChangedAction{
		Type: ahptypes.ActionTypeRootAgentsChanged, Agents: agents,
	}}, nil)
}
