// Package ahphost serves the discoboxes this client can reach to an Agent Host
// Protocol client — VS Code's Agent Sessions — as ADR 26-10-17-354 decides: a
// discobox is a session, each of its harness terminals a chat whose screen is a
// live terminal, and each shell the client opens an AHP terminal.
//
// The host holds every channel's state and changes it only through the
// protocol's own reducers (the SDK's ahp.ApplyActionTo*), the way a client
// does, so a snapshot is always exactly what a client folding the same actions
// would hold. What it knows of Discobox comes through Boxes, which the
// console's data source implements.
package ahphost

import (
	"context"
	"crypto/subtle"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// Boxes is what the host needs of Discobox. The console's data source
// (internal/cli's apiDataSource) implements it, across every server this
// client knows.
type Boxes interface {
	// List is every discobox on every server, with what its agent last
	// reported over OSC 7501.
	List(ctx context.Context) (tui.Listing, error)
	// Harnesses is what the primary server offers to run, when server is "".
	Harnesses(ctx context.Context, server string) ([]tui.Harness, error)
	// Run creates a discobox, as `discobox new` does.
	Run(ctx context.Context, req tui.RunRequest, report func(string), accepted func()) (tui.Sandbox, error)
	// Do archives, unarchives or starts a discobox.
	Do(ctx context.Context, verb tui.Verb, sandboxID string) error
	// Rename gives a discobox a name.
	Rename(ctx context.Context, sandboxID, name string) error
	// Execs lists a running discobox's terminals, shells and services.
	Execs(ctx context.Context, sandboxID string) ([]tui.Exec, error)
	// OpenExec attaches to an exec, replaying its screen. It never relaunches
	// an exec that has stopped when execID is the exec's own ID.
	OpenExec(ctx context.Context, sandboxID, execID string, cols, rows int) (tui.Terminal, error)
	// NewShell and NewTerminal create, attach and start a shell, or another
	// session of the discobox's harness.
	NewShell(ctx context.Context, sandboxID string, cols, rows int) (tui.Exec, tui.Terminal, error)
	NewTerminal(ctx context.Context, sandboxID string, cols, rows int) (tui.Exec, tui.Terminal, error)
	// EndExec ends an exec, killing what runs in it.
	EndExec(ctx context.Context, sandboxID, execID string) error
	// SendInput types text into an exec and presses Enter, as a paste when
	// the program asked for bracketed paste.
	SendInput(ctx context.Context, sandboxID, execID, text string) error
	// PutImage carries an image into the discobox and says where it put it.
	PutImage(ctx context.Context, sandboxID, name string, data []byte) (string, error)
}

// Config is a host's settings.
type Config struct {
	Boxes Boxes
	// Token is the connection token a client presents as the `tkn` query
	// parameter, which is how VS Code sends `connectionToken`.
	Token string
	// Source is the directory a session created without a folder is cut
	// from: the directory the host was started in.
	Source string
	// Version is this CLI's version, offered as the server's.
	Version string
	// Poll is how often the discoboxes are listed again.
	Poll time.Duration
	// Logf reports what the host cannot tell a client.
	Logf func(format string, args ...any)
}

// Host is one AHP host. Every field below mu is guarded by it; nothing that
// talks to a server is done while holding it.
type Host struct {
	cfg Config

	// ready is closed once the first listing has landed, which is what
	// initialize and listSessions wait for: a session list taken before then
	// would be empty rather than late.
	ready     chan struct{}
	readyOnce sync.Once
	// kicked asks Run for a poll ahead of the ticker.
	kicked chan struct{}

	mu       sync.Mutex
	seq      int64
	conns    map[*conn]struct{}
	root     ahptypes.RootState
	rootSubs map[*conn]struct{}
	sessions map[string]*session // by session URI
	// boxSessions is a discobox's session URI, by discobox ID: its own
	// `ahp-session:/<id>`, or the URI a client chose when it created the
	// session.
	boxSessions map[string]string
	chats       map[string]*chat     // by chat URI
	terminals   map[string]*terminal // by terminal URI
	// harnesses is what the primary offers, by slug, as the last poll found.
	harnesses map[string]tui.Harness

	// ctx is the host's lifetime, ended when Run returns. Work that outlives
	// the request that started it runs under it: an attach, which lives as
	// long as the context it was opened with, a message's delivery, a listing
	// a slow server is still answering. A request's context would end that
	// work with the request.
	ctx  context.Context
	stop context.CancelFunc
}

// New makes a host. Run polls for it; ServeHTTP serves its clients.
func New(cfg Config) *Host {
	if cfg.Poll <= 0 {
		cfg.Poll = 3 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Host{
		cfg:         cfg,
		ctx:         ctx,
		stop:        stop,
		ready:       make(chan struct{}),
		kicked:      make(chan struct{}, 1),
		conns:       map[*conn]struct{}{},
		root:        ahptypes.RootState{Agents: []ahptypes.AgentInfo{}},
		rootSubs:    map[*conn]struct{}{},
		sessions:    map[string]*session{},
		boxSessions: map[string]string{},
		chats:       map[string]*chat{},
		terminals:   map[string]*terminal{},
		harnesses:   map[string]tui.Harness{},
	}
}

// ServeHTTP accepts one client's WebSocket, once it has shown the token.
//
// Any origin is accepted: VS Code connects from an Electron renderer whose
// origin is its own, and the token, not the origin, is what admits a client.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("tkn")), []byte(h.cfg.Token)) != 1 {
		http.Error(w, "a connection token is required", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	// A message carries a pasted image as base64, and a screenshot is
	// routinely megabytes.
	ws.SetReadLimit(64 << 20)
	c := newConn(h, ws)
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	c.serve(r.Context())
	h.dropConn(c)
}

// waitReady waits for the first listing, for as long as ctx allows.
func (h *Host) waitReady(ctx context.Context) {
	select {
	case <-h.ready:
	case <-ctx.Done():
	}
}

// dispatch applies a server-side action to a channel's state and sends it to
// that channel's subscribers. origin is the client action it echoes, nil for
// the host's own. h.mu must be held.
func (h *Host) dispatch(channel string, action ahptypes.StateAction, origin *ahptypes.ActionOrigin) {
	subs := h.applyLocked(channel, action)
	h.seq++
	env := ahptypes.ActionEnvelope{Channel: channel, Action: action, ServerSeq: h.seq, Origin: origin}
	for c := range subs {
		c.notify("action", env)
	}
}

// reject tells the client that sent action that it was not applied. Nothing
// changes, but the sequence moves: the envelope is one the client counts.
func (h *Host) reject(channel string, action ahptypes.StateAction, origin *ahptypes.ActionOrigin, reason string) {
	h.seq++
	env := ahptypes.ActionEnvelope{Channel: channel, Action: action, ServerSeq: h.seq, Origin: origin, RejectionReason: &reason}
	for c := range h.subscribers(channel) {
		c.notify("action", env)
	}
}

// notifyRoot sends a protocol notification to the root channel's subscribers.
func (h *Host) notifyRoot(method string, params any) {
	for c := range h.rootSubs {
		c.notify(method, params)
	}
}

// subscribers is a channel's subscriber set, nil for a channel that does not
// exist. h.mu must be held.
func (h *Host) subscribers(channel string) map[*conn]struct{} {
	if channel == ahptypes.RootResourceURI {
		return h.rootSubs
	}
	if s := h.sessions[channel]; s != nil {
		return s.subs
	}
	if c := h.chats[channel]; c != nil {
		return c.subs
	}
	if t := h.terminals[channel]; t != nil {
		return t.subs
	}
	return nil
}

// dropConn forgets a client that has gone, and lets go of the terminals
// nobody is watching any more.
func (h *Host) dropConn(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.conns, c)
	delete(h.rootSubs, c)
	for _, s := range h.sessions {
		delete(s.subs, c)
	}
	for _, ch := range h.chats {
		delete(ch.subs, c)
	}
	changed := false
	for uri, t := range h.terminals {
		delete(t.subs, c)
		h.releaseLocked(t)
		// A shell this client opened that has exited and that nobody watches
		// any more is forgotten — another client's, never: VS Code opens one by itself on every connect and need not
		// dispose of it, and kept, they would pile up for the host's life.
		if t.owner == c && t.shell() && t.exited() && len(t.subs) == 0 {
			delete(h.terminals, uri)
			changed = true
		}
	}
	if changed {
		h.terminalsChangedLocked()
	}
	// A draft this client created and never sent a message to is forgotten
	// once nobody has it open — another client's, never: it has no discobox, and VS Code makes
	// one for every new session it shows.
	for _, s := range h.sessions {
		if s.owner == c && !s.hasBox && !s.creating && !h.watchedLocked(s) {
			h.removeSessionLocked(s)
		}
	}
}

// now is the protocol's timestamp: ISO 8601 in UTC with milliseconds.
func now() string { return timestamp(time.Now()) }

func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
