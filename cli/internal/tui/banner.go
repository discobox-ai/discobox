package tui

import (
	"slices"
	"time"

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

// The credential band's call to action throbs. It is the one animated thing in
// the window, and it is animated because it is the one thing on screen that
// somebody is waiting on: an agent has stopped, and every second it stays
// stopped is a second of nothing happening. The offer's chip is still, so that
// the moving one means what it says.
//
// bannerPulseHues is the field the chip steps through, up and back down. Four
// frames, held long enough to read as a heartbeat rather than as a blink — a
// bar that flashes is one the eye learns to look past, which is the whole
// failure this is trying to avoid.
var bannerPulseHues = []string{colAlertChip, colAlertMid, colAlertLit, colAlertMid}

// bannerPulseInterval is how long each beat is held: a whole throb, up and back
// down, is four of them, about a second and a half.
const bannerPulseInterval = 400 * time.Millisecond

type bannerPulseMsg struct{ gen int }

// armBannerPulse starts the throb when the credential band comes up and stops
// it when it goes, whatever it was that put it there or took it away.
//
// It is called from the one place every message passes through rather than from
// each of the several things that can raise or answer a request — a request
// arriving, one being answered, a pane being switched, a workspace being closed
// — because a clock left running on a bar that is no longer drawn is a window
// that never idles, and a bar drawn with no clock behind it is a button that
// sits still while somebody waits.
//
// The generation is what makes that safe: a tick names the run it belongs to,
// so a band that goes and comes back is one clock rather than two beating
// against each other.
func (m *Model) armBannerPulse() tea.Cmd {
	want := m.st.color && m.bannerShowing() == bannerCredential
	if want == m.pulsing {
		return nil
	}
	m.pulsing, m.pulseGen = want, m.pulseGen+1
	if !want {
		m.pulse = 0
		return nil
	}
	return bannerPulseTick(m.pulseGen)
}

func bannerPulseTick(gen int) tea.Cmd {
	return tea.Tick(bannerPulseInterval, func(time.Time) tea.Msg {
		return bannerPulseMsg{gen: gen}
	})
}

// advanceBannerPulse moves the throb on a beat. A tick from a run that is over
// is dropped rather than answered: the band it was beating for is gone.
func (m *Model) advanceBannerPulse(msg bannerPulseMsg) tea.Cmd {
	if !m.pulsing || msg.gen != m.pulseGen {
		return nil
	}
	m.pulse++
	return bannerPulseTick(m.pulseGen)
}

// bannerRow paints one band: the mark and its sentence on the left, the call to
// action centered in the window, the key that acts pinned to the right.
//
// The bar is painted and the text keeps its own colors over it — the mark that
// catches the eye, the subject, the call, and the key — because a whole bar
// drawn in reverse video is a slab at a glance and a struggle to read at a
// sentence.
//
// The call is the one thing on the bar that is not a statement, so it is not
// left at the end of the sentence where a reader who has already stopped seeing
// the header will never reach it. It sits in the middle of the row itself
// rather than in the gap between the other two, so it does not slide about as
// the subject changes length, and it is drawn as a chip — its own field, inside
// the band's — because the whole band is a button and nothing about a bar of
// flat color says so.
//
// The key is pinned: on a narrow window the subject is what gives way, because
// a bar that says something is there and not what to press about it is a bar
// that has said the less useful half. The call goes before the key does, whole
// rather than cut: a chip reading "click to ap…" is a button with a typo on it.
// The key's "or" goes with it, because it is the chip the key is the other way
// of doing. The two cells of band in front of the key are part of the right, so
// the gap survives a subject long enough to be cut back against it.
func bannerRow(st *styles, width int, mark lipgloss.Style, glyph, subject, call, key, bg string) string {
	head := mark.Render(" " + glyph + "  ")
	keyed := st.attentionText.Render(key) + st.attentionHint.Render(" ")
	right := st.attentionHint.Render("  or  ") + keyed
	// The mark whole, two cells of air, the chip, and a cell before the key's
	// own gap — spreadCenterPin keeps each of them — then the key. Less room
	// than that and the call goes, rather than the mark or the key.
	if call == "" || lipgloss.Width(head)+3+lipgloss.Width(call)+lipgloss.Width(right) > width {
		call, right = "", st.attentionHint.Render("  ")+keyed
	}
	return highlight(st, padANSI(spreadCenterPin(head+subject, call, right, width), width), bg)
}

// bannerChip is a call to action drawn as a button: bold text in a field of its
// own, with two cells of that field on either side of the words so it reads as
// something pressable rather than as a highlighted phrase.
//
// Without color there is no field, and what is left is the sentence — which is
// why the words say the gesture rather than naming a button.
func bannerChip(st *styles, text, fg, bg string) string {
	if !st.color {
		return text
	}
	return lipgloss.NewStyle().Bold(true).
		Foreground(lipgloss.Color(fg)).
		Background(lipgloss.Color(bg)).
		Render("  " + text + "  ")
}
