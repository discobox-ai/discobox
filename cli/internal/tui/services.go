package tui

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The workspace's view of the discobox's declared services (ADR 0070).
//
// A running service is already on screen — it arrives in the exec listing like
// any other session and is drawn as a tab on the right — so what this adds is
// the half a tab cannot show: the services that are *not* running, and the
// three verbs. Both live behind one leader key, because a service is something
// you set going and then stop thinking about, not something you work in.
//
// The menu is opened from the server every time rather than cached. A service
// stopped from another window, or one whose file was added to the repository a
// minute ago, is exactly what it is being opened to find out about.

// Service is one of a discobox's declared services, as the menu draws it.
type Service struct {
	ID          string
	Name        string
	Description string
	// Status is the sandbox's own word for what the service is doing:
	// stopped, starting, running, exited or failed.
	Status string
	// Problem is why a declaration cannot run — a missing shebang or
	// executable bit. Such a service is listed, because the alternative is a
	// file the author believes is a service and that nothing ever mentions.
	Problem string

	// FileName is the declaring file, which is what a pane points at when it
	// has to say where to go and fix something.
	FileName string
	// ExecID is the exec running, or last run, for this service. Empty when
	// none ever has.
	ExecID string
	// StartedAt is when the current or last run began. With ExecID it says
	// whether a pane is looking at the run the server is reporting: a restart
	// keeps the exec id (ADR 0038) and moves this.
	StartedAt time.Time
	// ExitCode is how the last run ended, absent when it has not ended.
	ExitCode *int
	// Error is what the last run failed with, when it did.
	Error string
}

// runnable reports whether this declaration can be acted on at all.
func (s Service) runnable() bool { return strings.TrimSpace(s.Problem) == "" }

// detail is the row's second line: why it cannot run, or what it is for.
func (s Service) detail() string {
	if !s.runnable() {
		return "cannot run: " + s.Problem
	}
	if description := strings.TrimSpace(s.Description); description != "" {
		return s.Status + " · " + description
	}
	return s.Status
}

// displayName is what the service is called on screen.
func (s Service) displayName() string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}
	return s.ID
}

// servicesMsg is the answer to opening the menu: the discobox's services, or
// why they could not be read.
type servicesMsg struct {
	gen      int
	services []Service
	err      error
}

// serviceMenuMsg opens the verb menu for one service. It is a message rather
// than a direct call because a dialog's action returns a command, and the
// command is the only way one dialog can put another in its place.
type serviceMenuMsg struct{ service Service }

// serviceVerbMsg runs one verb against one service.
type serviceVerbMsg struct {
	verb    ServiceVerb
	service Service
}

// openServices asks for the discobox's services and opens the menu on them.
func (m *Model) openServices() tea.Cmd {
	if m.paneBox.ID == "" {
		return nil
	}
	m.busy = "services…"
	gen := m.wsGen
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	return func() tea.Msg {
		services, err := ds.Services(ctx, id)
		return servicesMsg{gen: gen, services: services, err: err}
	}
}

// showServices puts the service list on screen. A discobox that declares none
// says so rather than opening an empty menu: an empty list of things you have
// never heard of reads as a failure.
func (m *Model) showServices(msg servicesMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "services: %v", msg.err)
	}
	if len(msg.services) == 0 {
		return m.report(false, "this discobox declares no services — add one under .discobox/services")
	}
	items := make([]action, 0, len(msg.services))
	for i, service := range msg.services {
		items = append(items, action{
			// The key is the row's number, so the first nine are reachable by
			// digit as well as by moving to them — the same arrangement the
			// harness file picker uses.
			key:     itoa(i + 1),
			label:   service.displayName(),
			detail:  service.detail(),
			enabled: service.runnable(),
			why:     service.Problem,
		})
	}
	services := msg.services
	m.dialog = actionsDialog("Services — "+m.paneBox.Name, "", items, func(key string) tea.Cmd {
		for i, service := range services {
			if itoa(i+1) == key {
				return func() tea.Msg { return serviceMenuMsg{service: service} }
			}
		}
		return nil
	})
	return nil
}

// showServiceMenu is the second half: what to do with the service just chosen.
//
// All three verbs are always offered. The sandbox settles what each one means
// for the state the service is actually in — starting a running service
// changes nothing, stopping a stopped one likewise — and a menu that greys out
// the verb you came for, on a status read a moment ago, is a menu arguing with
// a service that has since moved on.
func (m *Model) showServiceMenu(service Service) tea.Cmd {
	items := []action{
		{key: "s", label: "start", detail: "run it, or leave it running", enabled: true},
		{key: "t", label: "stop", detail: "end its run and keep what it printed", enabled: true},
		{key: "R", label: "restart", detail: "stop it if it is running, then start it", enabled: true},
	}
	m.dialog = actionsDialog(service.displayName(), service.detail(), items, func(key string) tea.Cmd {
		verb := ServiceRestart
		switch key {
		case "s":
			verb = ServiceStart
		case "t":
			verb = ServiceStop
		}
		return func() tea.Msg { return serviceVerbMsg{verb: verb, service: service} }
	})
	return nil
}

// runServiceVerb acts on one service and reports the outcome on the status
// line. What the service does next arrives through the exec listing the
// workspace is already polling, so there is nothing to refresh here: a service
// that starts becomes a tab on its own, and one that stops takes its tab with
// it.
func (m *Model) runServiceVerb(verb ServiceVerb, service Service) tea.Cmd {
	if m.paneBox.ID == "" {
		return nil
	}
	m.busy = string(verb) + "…"
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	name := service.displayName()
	return func() tea.Msg {
		err := ds.DoService(ctx, verb, id, service.ID)
		return serviceDoneMsg{verb: verb, name: name, err: err}
	}
}

// serviceDoneMsg is one finished verb.
type serviceDoneMsg struct {
	verb ServiceVerb
	name string
	err  error
}

func (m *Model) serviceDone(msg serviceDoneMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "cannot %s %s: %v", msg.verb, msg.name, msg.err)
	}
	return m.report(false, "%s", msg.verb.done(msg.name))
}

// A service's pane is keyed by the service rather than by the exec running it.
// The exec is the run; the service is the thing, and it outlives any run of
// itself — stopped, restarted under the same id (ADR 0038), or never started at
// all. Keying on the service is what lets one tab follow it through all of
// that, and what lets a service with no exec have a tab at all.
func servicePaneID(id string) string { return "service:" + id }

// paneWorthy reports whether a service should have a tab.
//
// Running is obvious. The rest is the answer to "would its absence be a
// surprise": a service that failed, one that ended on its own, and a
// declaration that cannot run are all things you would otherwise learn about
// only by pressing a key you had no reason to press. A service you stopped
// yourself is the one case where absence says the right thing — you know, and a
// tab you have to dismiss every time would be the window nagging.
func (s Service) paneWorthy() bool {
	switch {
	case !s.runnable():
		return true
	case s.live():
		return true
	case s.Status == "failed", s.Status == "exited":
		return true
	default:
		return false
	}
}

// live reports whether there is a running process to attach to.
func (s Service) live() bool { return s.Status == "running" || s.Status == "starting" }

// runKey identifies the run a pane is looking at, so the workspace can notice
// when it is looking at a stale one. A restart keeps the exec id and moves the
// start time; a stop clears the liveness; a fixed declaration clears the
// problem. Any of those means the pane has to be opened again.
func (s Service) runKey() string {
	return fmt.Sprintf("%s|%s|%s|%s", s.ExecID, s.StartedAt.UTC().Format(time.RFC3339Nano), s.Status, s.Problem)
}

// tabMark is the sign the tab strip wears beside a service that is not running:
// enough to tell "it is fine" from "go and look" without opening it.
func (s Service) tabMark() string {
	switch {
	case !s.runnable(), s.Status == "failed":
		return " ✗"
	case s.Status == "exited":
		return " ·"
	default:
		return ""
	}
}

// paneStatus is the word the pane's own header wears for a service that is not
// running, so the state is readable without reading the card.
func (s Service) paneStatus() string {
	if !s.runnable() {
		return "cannot run"
	}
	return s.Status
}

// card is what a pane draws for a service with no stream of its own: what it
// is, what state it is in, why, and then whatever its last run printed.
//
// It is written as terminal output rather than laid out with the window's
// styles because that is what it is — the pane is a terminal, and this is the
// one place the window writes into one. CRLF throughout: the pane sets LNM for
// a service, but this text is not the service's and should not depend on it.
func (s Service) card(logs []byte) []byte {
	var b strings.Builder
	b.WriteString(s.displayName() + "\r\n")
	b.WriteString(strings.Repeat("─", len([]rune(s.displayName()))) + "\r\n\r\n")
	switch {
	case !s.runnable():
		b.WriteString("cannot run\r\n\r\n")
		b.WriteString(wrapCard(s.Problem) + "\r\n\r\n")
		b.WriteString(".discobox/services/" + s.FileName + "\r\n")
	case s.Status == "failed":
		b.WriteString("failed\r\n\r\n")
		if detail := strings.TrimSpace(s.Error); detail != "" {
			b.WriteString(wrapCard(detail) + "\r\n\r\n")
		} else if s.ExitCode != nil {
			fmt.Fprintf(&b, "exit code %d\r\n\r\n", *s.ExitCode)
		}
	case s.Status == "exited":
		b.WriteString("exited\r\n\r\n")
		if s.ExitCode != nil {
			fmt.Fprintf(&b, "exit code %d\r\n\r\n", *s.ExitCode)
		}
	default:
		b.WriteString(s.Status + "\r\n\r\n")
	}
	if description := strings.TrimSpace(s.Description); description != "" {
		b.WriteString(wrapCard(description) + "\r\n\r\n")
	}
	if len(logs) > 0 {
		b.WriteString("── last output ──\r\n\r\n")
		// The transcript is the program's own bytes, line feeds and all. The
		// pane's LNM draws them as newlines, the same way the running service's
		// output is drawn.
		b.Write(logs)
		if !strings.HasSuffix(string(logs), "\n") {
			b.WriteString("\r\n")
		}
	}
	return []byte(b.String())
}

// cardWidth is what the card wraps its prose at. It is a fixed, narrow measure
// rather than the pane's width: the card is read, not filled, and a reason
// running the width of a maximized window is harder to read than one that does
// not.
const cardWidth = 64

func wrapCard(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if len([]rune(line))+1+len([]rune(word)) > cardWidth {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	return strings.Join(append(lines, line), "\r\n")
}

// textTerminal is a Terminal over a fixed block of text: the card a service
// with no running process is drawn from.
//
// It is a stream rather than a special kind of pane because a pane already
// knows how to draw a stream, scroll it, and let it be selected and copied.
// Inventing a second kind of pane to show text would mean teaching the tab
// strip, the focus, the mouse and the layout about it; handing the existing one
// some bytes teaches them nothing.
type textTerminal struct {
	text   []byte
	events chan TerminalEvent
	done   chan struct{}
	once   sync.Once
}

func newTextTerminal(text []byte) *textTerminal {
	return &textTerminal{text: text, events: make(chan TerminalEvent), done: make(chan struct{})}
}

// Read delivers the text once and then blocks until close.
//
// It blocks rather than reporting EOF because EOF is an ending, and the pane
// treats one as a session that finished: it would mark the pane exited and
// offer to dismiss it. Nothing ended here — the service is simply not running,
// which is the state the pane is reporting — so the stream stays open and the
// pane stays a pane until the workspace replaces it.
func (t *textTerminal) Read(p []byte) (int, error) {
	if len(t.text) > 0 {
		n := copy(p, t.text)
		t.text = t.text[n:]
		return n, nil
	}
	<-t.done
	return 0, io.EOF
}

// Write drops what it is given. A read-only pane sends nothing, so the only
// writer here is the emulator answering a query about itself, and the answer
// has nowhere to go.
func (t *textTerminal) Write(p []byte) (int, error) { return len(p), nil }

func (t *textTerminal) Resize(int, int) error { return nil }

func (t *textTerminal) Events() <-chan TerminalEvent { return t.events }

func (t *textTerminal) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}

// historyTerminal plays a block of history and then hands over to a live
// stream. It is what a service's pane is opened on: a plain exec has no screen
// to repaint from, so attaching to a running service starts at "now" and the
// pane sits empty until the service says something next — which for a server
// that has finished booting can be a long time.
//
// Writes, resizes, events and close all belong to the live stream. Only Read is
// interposed.
type historyTerminal struct {
	history []byte
	Terminal
}

func (t *historyTerminal) Read(p []byte) (int, error) {
	if len(t.history) > 0 {
		n := copy(p, t.history)
		t.history = t.history[n:]
		return n, nil
	}
	return t.Terminal.Read(p)
}

// historyLimit bounds how much of a transcript is played into a pane on open.
//
// A dev server left running for a day has a transcript far longer than anything
// worth reading, and every byte of it is parsed by the emulator before the pane
// draws. The cap is generous enough to hold what you would scroll back through
// and small enough that opening a workspace does not stall on it.
const historyLimit = 256 << 10

// tailHistory is the last of a transcript, cut at a line boundary so the first
// line drawn is a whole one — and, more to the point, so the cut never lands
// inside an escape sequence and leaves the emulator interpreting the remainder
// as text.
func tailHistory(logs []byte) []byte {
	if len(logs) <= historyLimit {
		return logs
	}
	tail := logs[len(logs)-historyLimit:]
	if at := bytes.IndexByte(tail, '\n'); at >= 0 && at+1 < len(tail) {
		return tail[at+1:]
	}
	return tail
}
