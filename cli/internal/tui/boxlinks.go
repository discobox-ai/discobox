package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// A discobox:// URI in a pane is a link the window answers itself
// (ADR 26-11-03-113 §3): pressing it focuses that box. Nothing outside the
// window can open one, so the pane draws it with no OSC 8 for the terminal to
// follow (termpane.WithClaimedLinks), and the window marks every one the pane
// reports, lights it under the pointer, and takes a press on it, plain or
// with Ctrl held, before the program in the pane is told of it.

// boxLinkServerMsg is which listed server a box link's server half names.
type boxLinkServerMsg struct {
	uri     string
	sandbox string
	server  string
	// err is why the link names no server this window lists.
	err error
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
// focused (landOn), or why not. Which box that is, is the core's
// (console.ParseBoxLink, BoxLinkServer, AddressedBox).
func (m *Model) pressBoxLink(p *pane, uri string, x, y int) tea.Cmd {
	m.boxLinkPress, m.boxLinkClicked = true, true
	m.countClick(x, y)
	m.clearSelections()
	address, err := console.ParseBoxLink(uri)
	if err != nil {
		return say("%v", err)
	}
	if address.Local() {
		return m.focusAddressed(m.paneServer(p), address.Sandbox, uri)
	}
	ctx, ds, from := m.ctx, m.ds, m.boxRef(m.Box)
	return func() tea.Msg {
		server, err := console.BoxLinkServer(ctx, ds, address, "", uri)
		return boxLinkServerMsg{uri: uri, sandbox: address.Sandbox, server: server, err: err, from: from}
	}
}

// boxLinkServer carries a box link on once its server is known.
func (m *Model) boxLinkServer(msg boxLinkServerMsg) tea.Cmd {
	// An answer that took its time is for the screen it was asked from: a
	// person who has moved to another box since is not sent back.
	if m.boxRef(m.Box) != msg.from {
		return nil
	}
	if msg.err != nil {
		return say("%v", msg.err)
	}
	return m.focusAddressed(msg.server, msg.sandbox, msg.uri)
}

// paneServer is the reader's server for a box link in p
// (console.LinkReaderServer).
func (m *Model) paneServer(p *pane) string {
	inBox := p != nil && p.sandbox.ID != ""
	server := ""
	if inBox {
		server = m.boxByRef(m.boxRef(p.sandbox)).Server
	}
	primary := ""
	if servers := m.list.Session.Servers; len(servers) > 0 {
		primary = servers[0]
	}
	return console.LinkReaderServer(server, inBox, primary)
}

// focusAddressed focuses the discobox ref names on server
// (console.AddressedBox), from what the listing last said.
func (m *Model) focusAddressed(server, ref, uri string) tea.Cmd {
	box, err := console.AddressedBox(m.list.All(), m.list.Unreachable(), server, ref, uri)
	switch {
	case err != nil:
		return say("%v", err)
	case m.oneShot():
		// A window that is one command's own shows its one box and never
		// the list (REVIEW.md), so it switches to no other: it says which
		// box the link is, and how to open it.
		if m.boxRef(box) == m.boxRef(m.Box) {
			return nil
		}
		on := "this server"
		if server != "" {
			on = server
		}
		return say("%q is %s on %s — this window is one command's; `discobox attach %q` opens it", uri, box.Label(), on, box.ID)
	}
	return m.landOn(box)
}
