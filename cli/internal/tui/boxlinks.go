package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/endpoint"
)

// A discobox:// URI in a pane is a link the window answers itself
// (ADR 26-11-03-113 §3): pressing it focuses that box. Nothing outside the
// window can open one, so the pane draws it with no OSC 8 for the terminal to
// follow (termpane.WithClaimedLinks), and the window marks every one the pane
// reports, lights it under the pointer, and takes a press on it, plain or
// with Ctrl held, before the program in the pane is told of it.

// boxLinkSchemes are the schemes a discobox's address is written in, which
// every pane claims.
var boxLinkSchemes = []string{endpoint.SchemeDiscobox, endpoint.SchemeDiscoboxHTTP, endpoint.SchemeDiscoboxHTTPS}

// addressedServerTimeout bounds asking which server a box link names. Most of
// the answer is a comparison; what can be slow is a listed server asked for
// its peer ID, and a press should say something rather than hang.
const addressedServerTimeout = 5 * time.Second

// boxLinkServerMsg is which listed server a box link's server half names.
type boxLinkServerMsg struct {
	uri     string
	sandbox string
	server  string
	known   bool
	// unsure names the listed servers that did not say who they are, when
	// the link matched none of the rest.
	unsure error
	// from is the box on screen when the link was pressed.
	from console.BoxRef
}

// markBoxLinks marks the box links a pane's last View drew, at the origin its
// grid is drawn at, and lights the one under the pointer. A seized mouse is a
// selection everywhere (leader m), so then they are neither: the text is
// there to be copied.
func (m *Model) markBoxLinks(p *pane, lines []string, x, y int) {
	if m.mouseSeized {
		return
	}
	for _, link := range p.term.Links() {
		if link.Row < 0 || link.Row >= len(lines) {
			continue
		}
		m.zones.mark(boxLinkHit(link.URI), x+link.Col, y+link.Row, link.Width, 1)
		if m.zones.hovering(x+link.Col, y+link.Row, link.Width, 1) {
			lines[link.Row] = litCells(m.st.hover.Render, lines[link.Row], link.Col, link.Width)
		}
	}
}

// litCells redraws width cells of a rendered line from col in the hover
// style, from their bare text, leaving what is either side as it was drawn.
func litCells(render func(...string) string, line string, col, width int) string {
	before := ansi.Truncate(line, col, "")
	cells := ansi.Strip(ansi.Cut(line, col, col+width))
	return before + ansi.ResetStyle + render(cells) + ansi.TruncateLeft(line, col+width, "")
}

// boxLinkEcho reports whether a left press in a pane is the second half of a
// double click on a box link, by the window's own click counting
// (countClick). The first press already switched the screen, so the second
// lands on whatever is drawn there now, and must not be taken as a press on
// it.
func (m *Model) boxLinkEcho(x, y int) bool {
	if !m.boxLinkClicked {
		return false
	}
	m.boxLinkClicked = false
	return m.countClick(x, y) > 1
}

// say is a status line about a box link. The link, and everything parsed out
// of it, is text the program in the pane wrote, and the status line is drawn
// outside any pane's emulator: so every such value goes in quoted, %q, whose
// escapes are the ones terminalSafe uses for C0 and C1 controls and invisible
// format characters (cli/REVIEW.md). A percent-encoded ESC in a link decodes
// to a real one.
func say(format string, args ...any) tea.Cmd {
	return func() tea.Msg { return statusMsg{text: fmt.Sprintf(format, args...), err: true} }
}

// pressBoxLink answers a press on a box link in p: the discobox it names,
// focused (landOn), or why not.
func (m *Model) pressBoxLink(p *pane, uri string, x, y int) tea.Cmd {
	m.boxLinkPress, m.boxLinkClicked = true, true
	m.countClick(x, y)
	m.clearSelections()
	address, ok, err := endpoint.ParseSandboxAddress(uri)
	if !ok || err != nil {
		// ParseSandboxAddress quotes what it was given in its errors.
		if err == nil {
			return say("%q is not a discobox's address", uri)
		}
		return say("%v", err)
	}
	if address.Local() {
		return m.focusAddressed(m.paneServer(p), address.Sandbox, uri)
	}
	ctx, ds, from := m.ctx, m.ds, m.boxRef(m.paneBox)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, addressedServerTimeout)
		defer cancel()
		name, known, err := ds.AddressedServer(ctx, address.Server)
		return boxLinkServerMsg{uri: uri, sandbox: address.Sandbox, server: name, known: known, unsure: err, from: from}
	}
}

// boxLinkServer carries a box link on once its server is known.
func (m *Model) boxLinkServer(msg boxLinkServerMsg) tea.Cmd {
	// An answer that took its time is for the screen it was asked from: a
	// person who has moved to another box since is not sent back.
	if m.boxRef(m.paneBox) != msg.from {
		return nil
	}
	switch {
	case msg.known:
		return m.focusAddressed(msg.server, msg.sandbox, msg.uri)
	case msg.unsure != nil:
		// It may be one of the servers that did not say who they are.
		return say("%q: no server this console lists says it is the one named, and %v", msg.uri, msg.unsure)
	}
	// Never dialed, never registered: the text was written by whatever runs
	// in the box, and a click is no reason to reach a host it chose. A typed
	// attach is (ADR 0116 §6).
	return say("%q is on a server this console doesn't know — `discobox attach %q` registers it", msg.uri, msg.uri)
}

// paneServer is the reader's server for a box link in p (ADR 26-11-03-113
// §2): the server of the box the pane belongs to, which is where the program
// that wrote it runs, and the primary for a pane that is no box's. A window
// with one server names none, on its rows or in its session.
func (m *Model) paneServer(p *pane) string {
	if p != nil && p.sandbox.ID != "" {
		return m.boxByRef(m.boxRef(p.sandbox)).Server
	}
	if servers := m.list.Session.Servers; len(servers) > 0 {
		return servers[0]
	}
	return ""
}

// focusAddressed focuses the discobox ref names on server, by ID or by a
// prefix only one of its discoboxes has, from what the listing last said.
func (m *Model) focusAddressed(server, ref, uri string) tea.Cmd {
	var found []console.Sandbox
	for _, box := range m.list.All() {
		if box.Server != server {
			continue
		}
		if box.ID == ref {
			found = []console.Sandbox{box}
			break
		}
		if strings.HasPrefix(box.ID, ref) {
			found = append(found, box)
		}
	}
	on := "this server"
	if server != "" {
		on = server
	}
	switch {
	case len(found) == 1 && m.oneShot():
		// A window that is one command's own shows its one box and never
		// the list (REVIEW.md), so it switches to no other: it says which
		// box the link is, and how to open it.
		if m.boxRef(found[0]) == m.boxRef(m.paneBox) {
			return nil
		}
		return say("%q is %s on %s — this window is one command's; `discobox attach %q` opens it", uri, sandboxLabel(found[0]), on, found[0].ID)
	case len(found) == 1:
		return m.landOn(found[0])
	case len(found) > 1:
		return say("%q: %q is more than one discobox on %s", uri, ref, on)
	case slices.Contains(m.list.Unreachable(), server):
		return say("%q: %s isn't answering", uri, on)
	}
	return say("%q: no discobox %q on %s", uri, ref, on)
}
