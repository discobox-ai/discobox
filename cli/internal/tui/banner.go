package tui

import (
	"slices"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The workspace's attention band: the one thing this screen draws that is not
// the terminal you came here to watch.
//
// There are two of them and they are the same object — a bar painted across the
// window, under the header and again above the keys, that says one thing and
// does that one thing when it is pressed. A credential request is a person
// being waited on (credentials.go); work that is ready to apply is an offer
// (apply.go). What they share is the geometry, the paint, and the rule that the
// key is pinned and the subject gives way, so they share the code for all
// three: two bars that drift apart in where they sit are two bars that put the
// hardware cursor in different wrong places.

// bannerKind is which band the workspace is showing. The order is the
// precedence, and only one is ever on screen: an agent blocked on a person
// outranks an offer that will still be there in a minute, and a screen with two
// exception bars on it has a header rather than an exception.
type bannerKind int

const (
	bannerNone bannerKind = iota
	bannerCredential
	bannerApply
)

// bannerSpan is where the band sits on screen, in absolute cells, both ends
// inclusive. It is recorded as the band is drawn — the same way the tabs and
// the maximize controls record theirs — so a press can be matched against what
// is actually on the frame rather than against where it ought to be.
type bannerSpan struct {
	// kind is which band was drawn, so a press answers the bar that is on
	// screen rather than the one the model would compose now.
	kind bannerKind
	// rows is every row the band was drawn on. There are two: one under the
	// header and one above the status line, so the same bar is in reach
	// wherever the eye is on a screen that is mostly terminal.
	rows       []int
	start, end int
	live       bool
}

// bannerShowing is the band the workspace has, if any.
func (m *Model) bannerShowing() bannerKind {
	if !m.inPanes() {
		return bannerNone
	}
	switch {
	case len(m.requests[m.paneBox.ID]) > 0:
		return bannerCredential
	case m.applyReady():
		return bannerApply
	}
	return bannerNone
}

// bannerTop is how many rows stand between the header and the boxes: the top
// band, or nothing.
//
// Everything that asks where something on screen *is* goes through this one —
// the hardware cursor, every mouse hit test — because they all measure down
// from the header, and only what is above the boxes moves them.
func (m *Model) bannerTop() int {
	if m.bannerShowing() == bannerNone {
		return 0
	}
	return 1
}

// bannerCost is how many rows the panes give up for the band: the one above
// them and the one below.
//
// It is a separate answer from bannerTop on purpose. The two were one number
// while there was one band, and a single number meaning both "how far down did
// the boxes move" and "how much shorter are they" is exactly the shape that
// puts a terminal's cursor a row away from the cell it is drawn in.
func (m *Model) bannerCost() int { return 2 * m.bannerTop() }

// bannerAt reports whether a press landed on either band.
func (m *Model) bannerAt(x, y int) bool {
	s := m.banner
	if !s.live || x < s.start || x > s.end {
		return false
	}
	return slices.Contains(s.rows, y)
}

// viewBanner draws whichever band the workspace has, and remembers which one it
// drew. Empty when there is none — and then the span goes too: a hit test left
// behind by a bar that is no longer there is a row of the header that silently
// acts on something nobody is looking at.
func (m *Model) viewBanner(width int) string {
	kind := m.bannerShowing()
	m.banner = bannerSpan{kind: kind}
	switch kind {
	case bannerCredential:
		return m.viewCredentialBanner(width)
	case bannerApply:
		return m.viewApplyBanner(width)
	}
	return ""
}

// pressBanner is what a click on the band asks for.
//
// It dispatches on what was drawn rather than on what the model would draw now,
// and the two bands answer a press differently on purpose: the credential band
// opens the question it is about, because answering it is the dialog. The apply
// band asks first — see confirmApply.
func (m *Model) pressBanner() tea.Cmd {
	switch m.banner.kind {
	case bannerCredential:
		return m.openCredentialDialog(m.paneBox.ID)
	case bannerApply:
		return m.confirmApply()
	}
	return nil
}

// bannerRow paints one band: the mark and its sentence on the left, the key
// that acts pinned to the right, over a field of bg.
//
// The bar is painted and the text keeps its own colors over it — the mark that
// catches the eye, the subject, and the key — because a whole bar drawn in
// reverse video is a slab at a glance and a struggle to read at a sentence.
//
// The key is pinned: on a narrow window the subject is what gives way, because
// a bar that says something is there and not what to press about it is a bar
// that has said the less useful half. The two cells of band in front of the key
// are part of the right, so the gap survives a subject long enough to be cut
// back against it.
func bannerRow(st *styles, width int, mark lipgloss.Style, glyph, subject, key, verb, bg string) string {
	left := mark.Render(" "+glyph+"  ") + subject
	right := st.attentionHint.Render("  ") + st.attentionText.Render(key) +
		st.attentionHint.Render("  or click to "+verb+" ")
	return highlight(st, padANSI(spreadPin(left, right, width), width), bg)
}
