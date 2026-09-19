package tui

import (
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The workspace's attention band: the one thing this screen draws that is not
// the terminal you came here to watch.
//
// There are four of them and they are the same object — a bar painted across
// the window, under the header and again above the keys, that says one thing and
// does that one thing when it is pressed. A credential request is a person
// being waited on (credentials.go); a refused credential is one that has to be
// replaced out here (rejections.go), and a harness with none bound is one that
// has to be configured out here (uncredentialed.go); work that is ready to
// apply is an offer (apply.go). What they share is the geometry, the paint, and
// the rule that the key is pinned and the subject gives way, so they share the
// code for all four: bars that drift apart in where they sit are bars that put the
// hardware cursor in different wrong places.

// bannerKind is which band the workspace is showing. The order is the
// precedence, and only one is ever on screen: a screen with two exception bars
// on it has a header rather than an exception.
//
// **A refused credential outranks the request an agent makes about it.** That
// ordering was the other way round at first, on the reasoning that somebody
// blocked on a keystroke right now comes before a credential that has been dead
// a while. In use it is backwards, because the two are usually the same event:
// the agent takes the 401, concludes it needs a credential, and asks for one —
// so the request *is* the symptom, and showing it hides the cause while
// offering the one action that cannot help. Handing over another credential
// leaves the dead one bound to the harness and the new one belonging to nobody;
// the fix is to replace what is already there.
//
// So the refusal stays up until it is dealt with, and everything else queues
// behind it. Nothing becomes unreachable by doing that: the leader keys for the
// request (credentialsLeaderKey) and the refusal (rejectedKey) are both bound
// whichever band is drawn, the list still marks a discobox with a request, and
// the secrets screen still lists them. Only the bar prioritizes.
type bannerKind int

const (
	bannerNone bannerKind = iota
	bannerRejected
	bannerUncredentialed
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
	// closeStart and closeEnd are the dismiss button's cells, both ends
	// inclusive, measured off the band as it was composed (viewBanner) and
	// placed at the origin the row was drawn at. closeEnd is below closeStart
	// when no button was drawn.
	closeStart, closeEnd int
	live                 bool
}

// bannerKinds is every band, in the order of precedence bannerKind states.
var bannerKinds = []bannerKind{bannerRejected, bannerUncredentialed, bannerCredential, bannerApply}

// bannerShowing is the band the workspace has, if any: the first that has
// something to say and has not been dismissed. Dismissing one shows what was
// queued behind it rather than an empty header.
func (m *Model) bannerShowing() bannerKind {
	if !m.inPanes() {
		return bannerNone
	}
	box := m.currentBox()
	for _, kind := range bannerKinds {
		if m.bannerWanted(kind, box) && !m.bannerDismissed(kind, box) {
			return kind
		}
	}
	return bannerNone
}

// bannerWanted is whether a band has something to say about this discobox on
// the screen as it is now — which, beyond whether it is true, can depend on
// what is drawn: neither the offer nor the signed-out harness speaks over the
// flow that is already answering it.
func (m *Model) bannerWanted(kind bannerKind, box Sandbox) bool {
	switch kind {
	case bannerRejected:
		return m.hasRejection(box)
	case bannerUncredentialed:
		return m.uncredentialed(box)
	case bannerCredential:
		return len(m.requests[box.ID]) > 0
	case bannerApply:
		return m.applyReady()
	}
	return false
}

// Dismissing a band.
//
// A dismissal is of one occurrence of a band, never of the band: dismissing a
// refused credential must not hide the next one, nor a request the one that
// arrives after it. So each band names its occurrence (bannerInstance) — the
// refusal by the credential, host and when it was first seen; a request by the
// requests waiting; the offer by the commit it would apply; the signed-out
// harness by the harness — and a dismissal holds the band down only while
// that is still what it would say.
//
// What it covered is forgotten the moment the window reads that it is over
// (pruneDismissed), which is what makes the signed-out harness's safe: its
// name for itself cannot change, but a harness configured and signed out again
// is read as over in between, and comes back. What is read once a poll can
// miss a change and its reversal inside one poll; nothing that is shorter than
// that is anything a person saw.
//
// Dismissals are the window's own. Nothing is sent to the server, and a new
// window shows every band that still applies.

// bannerDismissKey dismisses the band on screen, behind the leader: b, for
// the band.
const bannerDismissKey = "b"

// bannerClose is the band's own dismiss button, at its right end, after the
// key. Its width is the hit test's, so the two are one constant.
const bannerClose = " ✕ "

// dismissBannerMsg is the leader plus that key inside a pane.
type dismissBannerMsg struct{}

// dismissal is one band dismissed on one discobox. A band is about a discobox,
// and one dismissed on this box says nothing about the same harness or request
// seen from another.
type dismissal struct {
	box  string
	kind bannerKind
}

// dismissalFor is a band's key on this discobox, which is its server as well
// as its ID: IDs are only unique within a server.
func (m *Model) dismissalFor(kind bannerKind, box Sandbox) dismissal {
	return dismissal{box: m.serverName(box.Server) + "\x00" + box.ID, kind: kind}
}

// bannerInstance names the occurrence a band would be about, or nothing when
// there is none. It is what is true, without what is drawn: a band held down
// by the screen (bannerWanted) is still happening, and a dismissal of it stays.
//
// It is a set rather than one name because requests and refusals arrive and
// go one at a time: a dismissal covers the ones it saw, so one of them going
// leaves the rest dismissed, and a new one brings the band back — including a
// second refusal while the first is still outstanding.
func (m *Model) bannerInstance(kind bannerKind, box Sandbox) []string {
	switch kind {
	case bannerRejected:
		var names []string
		for _, rejection := range m.rejectionsFor(box) {
			names = append(names, rejection.occurrence())
		}
		return names
	case bannerUncredentialed:
		if box.HarnessUncredentialed && box.HarnessID != "" {
			return []string{box.HarnessID}
		}
	case bannerCredential:
		var ids []string
		for _, req := range m.requests[box.ID] {
			ids = append(ids, req.ID)
		}
		return ids
	case bannerApply:
		if box.ahead() {
			return []string{box.Git.Commit}
		}
	}
	return nil
}

// bannerDismissed is whether this band's occurrence on this discobox is one
// somebody dismissed.
func (m *Model) bannerDismissed(kind bannerKind, box Sandbox) bool {
	dismissed, ok := m.dismissed[m.dismissalFor(kind, box)]
	return ok && covers(dismissed, m.bannerInstance(kind, box))
}

// covers is whether every name in now was among those dismissed.
func covers(dismissed, now []string) bool {
	if len(now) == 0 {
		return false
	}
	for _, name := range now {
		if !slices.Contains(dismissed, name) {
			return false
		}
	}
	return true
}

// dismissBanner takes the band on screen down until what it is about changes.
func (m *Model) dismissBanner() tea.Cmd {
	kind := m.bannerShowing()
	if kind == bannerNone {
		return m.report(false, "no banner to dismiss")
	}
	box := m.currentBox()
	if m.dismissed == nil {
		m.dismissed = map[dismissal][]string{}
	}
	m.dismissed[m.dismissalFor(kind, box)] = m.bannerInstance(kind, box)
	// The band gives its two rows back to the panes — or hands them to the one
	// queued behind it, which costs the same.
	m.layout()
	return nil
}

// pruneDismissed forgets what each dismissal covered that is now over, and the
// dismissal once nothing it covered is left. It is run by everything that reads
// what the bands are about: the listing, the refused credentials, the
// credential inbox.
//
// It drops names rather than whole dismissals. Something new arriving beside
// what was dismissed does not end the dismissal of the rest: the band comes
// back for the new one (covers is false) and is about the new one
// (shownRejection), and what was dismissed stays dismissed. A dismissal the
// reads have not seen end is kept, including one whose discobox the listing is
// not showing — a server that has stopped answering has not settled anything.
func (m *Model) pruneDismissed() {
	for d, dismissed := range m.dismissed {
		box, ok := m.listedBox(d.box)
		if !ok {
			continue
		}
		now := m.bannerInstance(d.kind, box)
		kept := slices.DeleteFunc(slices.Clone(dismissed), func(name string) bool { return !slices.Contains(now, name) })
		if len(kept) == 0 {
			delete(m.dismissed, d)
			continue
		}
		m.dismissed[d] = kept
	}
}

// listedBox is the listing's row for a dismissal's discobox.
func (m *Model) listedBox(key string) (Sandbox, bool) {
	for _, box := range m.list.all {
		if m.serverName(box.Server)+"\x00"+box.ID == key {
			return box, true
		}
	}
	return Sandbox{}, false
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

// bannerCloseAt reports whether a press landed on the band's dismiss button,
// which is the last cells of the band: the key is pinned to the right and the
// button is pinned after it.
func (m *Model) bannerCloseAt(x, y int) bool {
	return m.bannerAt(x, y) && x >= m.banner.closeStart && x <= m.banner.closeEnd
}

// viewBanner draws whichever band the workspace has, and remembers which one it
// drew. Empty when there is none — and then the span goes too: a hit test left
// behind by a bar that is no longer there is a row of the header that silently
// acts on something nobody is looking at.
func (m *Model) viewBanner(width int) string {
	kind := m.bannerShowing()
	m.banner = bannerSpan{kind: kind, closeEnd: -1}
	var row string
	switch kind {
	case bannerRejected:
		row = m.viewRejectedBanner(width)
	case bannerUncredentialed:
		row = m.viewUncredentialedBanner(width)
	case bannerCredential:
		row = m.viewCredentialBanner(width)
	case bannerApply:
		row = m.viewApplyBanner(width)
	}
	// Where the button is, read off the row that was composed rather than
	// worked out again from how bannerRow lays one out: it is pinned last, so
	// it is the row's last cells when it made it onto the row at all. The
	// caller adds the origin it draws the row at.
	if strings.HasSuffix(ansi.Strip(row), bannerClose) {
		w := lipgloss.Width(row)
		m.banner.closeStart, m.banner.closeEnd = w-lipgloss.Width(bannerClose), w-1
	}
	return row
}

// pressBanner is what a click on the band asks for.
//
// It dispatches on what was drawn rather than on what the model would draw now,
// and the two bands answer a press differently on purpose: the credential band
// opens the question it is about, because answering it is the dialog. The apply
// band asks first — see confirmApply.
func (m *Model) pressBanner() tea.Cmd {
	switch m.banner.kind {
	case bannerRejected:
		return m.openRejectedRemedy(m.currentBox())
	case bannerUncredentialed:
		return m.openUncredentialedRemedy(m.currentBox())
	case bannerCredential:
		return m.openCredentialDialog(m.currentBox().ID)
	case bannerApply:
		return m.confirmApply()
	}
	return nil
}

// The credential band's call to action throbs. It is the one animated thing in
// the window, and it is animated because it is the one thing on screen that
// somebody is waiting on: an agent has stopped, and every second it stays
// stopped is a second of nothing happening. The offer's chip is still, and so
// is the refusal's, so that the moving one means what it says.
//
// A request waiting behind a refusal therefore does not throb — the refusal is
// drawn instead, and what is worth hurrying there is replacing the credential,
// not answering the question the dead one provoked.
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
	keyed := st.attentionText.Render(key) + st.attentionHint.Render(" ") + st.attentionText.Render(bannerClose)
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
