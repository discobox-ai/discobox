package console

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

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

// Runnable reports whether this declaration can be acted on at all.
func (s Service) Runnable() bool { return strings.TrimSpace(s.Problem) == "" }

// Detail is the row's second line: why it cannot run, or what it is for.
func (s Service) Detail() string {
	if !s.Runnable() {
		return "cannot run: " + s.Problem
	}
	if description := strings.TrimSpace(s.Description); description != "" {
		return s.Status + " · " + description
	}
	return s.Status
}

// DisplayName is what the service is called on screen.
func (s Service) DisplayName() string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}
	return s.ID
}

// PaneWorthy reports whether a service should have a tab.
//
// Running is obvious. The rest is the answer to "would its absence be a
// surprise": a service that failed, one that ended on its own, and a
// declaration that cannot run are all things you would otherwise learn about
// only by pressing a key you had no reason to press. A service you stopped
// yourself is the one case where absence says the right thing — you know, and a
// tab you have to dismiss every time would be the window nagging.
func (s Service) PaneWorthy() bool {
	switch {
	case !s.Runnable():
		return true
	case s.Live():
		return true
	case s.Status == "failed", s.Status == "exited":
		return true
	default:
		return false
	}
}

// Live reports whether there is a running process to attach to.
func (s Service) Live() bool { return s.Status == "running" || s.Status == "starting" }

// RunKey identifies the run a pane is looking at, so the workspace can notice
// when it is looking at a stale one. A restart keeps the exec id and moves the
// start time; a stop clears the liveness; a fixed declaration clears the
// problem. Any of those means the pane has to be opened again.
func (s Service) RunKey() string {
	return fmt.Sprintf("%s|%s|%s|%s", s.ExecID, s.StartedAt.UTC().Format(time.RFC3339Nano), s.Status, s.Problem)
}

// TabMark is the sign the tab strip wears beside a service that is not running:
// enough to tell "it is fine" from "go and look" without opening it.
func (s Service) TabMark() string {
	switch {
	case !s.Runnable(), s.Status == "failed":
		return " ✗"
	case s.Status == "exited":
		return " ·"
	default:
		return ""
	}
}

// PaneStatus is the word the pane's own header wears for a service that is not
// running, so the state is readable without reading the card.
func (s Service) PaneStatus() string {
	if !s.Runnable() {
		return "cannot run"
	}
	return s.Status
}

// NoServices is what a window says when asked for the services of a box that
// declares none, rather than opening an empty menu: an empty list of things
// you have never heard of reads as a failure.
const NoServices = "this discobox declares no services — add one under .discobox/services"

// ServiceAction is one verb a service's menu offers, as it stands for that
// service.
type ServiceAction struct {
	Verb    ServiceVerb
	Label   string
	Detail  string
	Enabled bool
	// Why is why it cannot act, when it cannot.
	Why string
}

// Actions is the verbs a service's menu offers, in its order.
//
// All three are offered whatever the service is doing. The sandbox settles
// what each one means for the state the service is actually in — starting a
// running service changes nothing, stopping a stopped one likewise — and a
// menu that greys out the verb you came for, on a status read a moment ago,
// is a menu arguing with a service that has since moved on. Only a
// declaration that cannot run refuses them, saying why.
func (s Service) Actions() []ServiceAction {
	out := []ServiceAction{
		{Verb: ServiceStart, Label: "start", Detail: "run it, or leave it running"},
		{Verb: ServiceStop, Label: "stop", Detail: "end its run and keep what it printed"},
		{Verb: ServiceRestart, Label: "restart", Detail: "stop it if it is running, then start it"},
	}
	for i := range out {
		out[i].Enabled = s.Runnable()
		if !out[i].Enabled {
			out[i].Why = s.Problem
		}
	}
	return out
}

// RunServiceVerb runs a verb on one of a box's services, as its menu offers it
// (Actions): the service is read again first, so a declaration that cannot
// run is refused in its own words, and one no longer declared is refused
// rather than sent. What the service does next arrives through the service
// listing; what is returned is what to say once it has acted (ServiceVerb.Done),
// and an error is worded to be said as it is.
func RunServiceVerb(ctx context.Context, ds DataSource, sandboxID, serviceID string, verb ServiceVerb) (string, error) {
	services, err := ds.Services(ctx, sandboxID)
	if err != nil {
		return "", fmt.Errorf("services: %w", err)
	}
	i := slices.IndexFunc(services, func(s Service) bool { return s.ID == serviceID })
	if i < 0 {
		return "", fmt.Errorf("this discobox declares no service %q", serviceID)
	}
	s := services[i]
	actions := s.Actions()
	j := slices.IndexFunc(actions, func(a ServiceAction) bool { return a.Verb == verb })
	switch {
	case j < 0:
		return "", fmt.Errorf("a service has no verb %q", verb)
	case !actions[j].Enabled:
		return "", fmt.Errorf("cannot %s %s: %s", verb, s.DisplayName(), actions[j].Why)
	}
	if err := ds.DoService(ctx, verb, sandboxID, s.ID); err != nil {
		return "", fmt.Errorf("cannot %s %s: %w", verb, s.DisplayName(), err)
	}
	return verb.Done(s.DisplayName()), nil
}

// OpenService opens what a service's tab draws: its running process, played
// in after the last of what it has written, when it has one; otherwise the
// card saying why there is none (serviceCard), with whatever its last run
// printed under it. The terminal is read-only, whichever it is.
func OpenService(ctx context.Context, ds DataSource, sandboxID string, s Service, cols, rows int) (Terminal, error) {
	// The transcript is read for both kinds of pane, and read first.
	//
	// A plain exec has no screen to repaint from, so attaching to a running
	// service starts at "now": without this the pane sits empty until the
	// service next says something, which for a server that has finished
	// booting is a long time. It is read before the attach rather than after,
	// so the seam between the two can only lose a moment of output rather
	// than repeat one — a repeated chunk reads as the service having done
	// something twice, which is worse than a gap.
	logs, err := ds.ServiceLogs(ctx, sandboxID, s.ID)
	if err != nil {
		// Not fatal to the pane: a live one still has its stream, and a card
		// without the transcript still says what happened, which is more than
		// an absent tab does.
		logs = nil
	}
	history := tailHistory(logs)
	if s.Live() && s.ExecID != "" {
		stream, err := ds.OpenExec(ctx, sandboxID, s.ExecID, cols, rows)
		if err != nil {
			return nil, err
		}
		return &historyTerminal{history: history, Terminal: stream}, nil
	}
	// A service that is not running has no stream, so the pane is handed the
	// one thing there is to say about it. Its output is part of that: after a
	// crash the output is the reason, and a reason you have to go and ask for
	// is one you find out about later than you needed to.
	return newTextTerminal(serviceCard(s, history)), nil
}

// serviceCard is what a pane draws for a service with no stream of its own:
// what it is, what state it is in, why, and then whatever its last run
// printed.
//
// It is written as terminal output rather than laid out with a window's
// styles because that is what it is — the pane is a terminal, and this is the
// one place the console writes into one. CRLF throughout: the text is not the
// service's, and should not depend on how its pane treats a line feed.
func serviceCard(s Service, logs []byte) []byte {
	var b strings.Builder
	b.WriteString(s.DisplayName())
	b.WriteString("\r\n")
	b.WriteString(strings.Repeat("─", len([]rune(s.DisplayName()))))
	b.WriteString("\r\n\r\n")
	switch {
	case !s.Runnable():
		b.WriteString("cannot run\r\n\r\n")
		b.WriteString(wrapCard(s.Problem))
		b.WriteString("\r\n\r\n")
		b.WriteString(".discobox/services/")
		b.WriteString(s.FileName)
		b.WriteString("\r\n")
	case s.Status == "failed":
		b.WriteString("failed\r\n\r\n")
		if detail := strings.TrimSpace(s.Error); detail != "" {
			b.WriteString(wrapCard(detail))
			b.WriteString("\r\n\r\n")
		} else if s.ExitCode != nil {
			fmt.Fprintf(&b, "exit code %d\r\n\r\n", *s.ExitCode)
		}
	case s.Status == "exited":
		b.WriteString("exited\r\n\r\n")
		if s.ExitCode != nil {
			fmt.Fprintf(&b, "exit code %d\r\n\r\n", *s.ExitCode)
		}
	default:
		b.WriteString(s.Status)
		b.WriteString("\r\n\r\n")
	}
	if description := strings.TrimSpace(s.Description); description != "" {
		b.WriteString(wrapCard(description))
		b.WriteString("\r\n\r\n")
	}
	if len(logs) > 0 {
		b.WriteString("── last output ──\r\n\r\n")
		// The transcript is the program's own bytes, line feeds and all; a
		// service's pane draws them as newlines, the same way the running
		// service's output is drawn.
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

// newTextTerminal is a Terminal that reads text once and then nothing more
// until it is closed.
func newTextTerminal(text []byte) Terminal {
	return &textTerminal{text: text, events: make(chan TerminalEvent), done: make(chan struct{})}
}

// Read delivers the text once and then blocks until close.
//
// It blocks rather than reporting EOF because EOF is an ending, and a pane
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

// Repaint has nothing to repaint from: the text is all there ever was, and the
// pane showing it is read-only.
func (t *textTerminal) Repaint() error { return nil }

func (t *textTerminal) Events() <-chan TerminalEvent { return t.events }

func (t *textTerminal) Close() error {
	t.once.Do(func() { close(t.done) })
	return nil
}

// historyTerminal plays a block of history and then hands over to a live
// stream. It is what a running service's pane is opened on: a plain exec has
// no screen to repaint from, so attaching to a running service starts at
// "now" and the pane sits empty until the service says something next — which
// for a server that has finished booting can be a long time.
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
