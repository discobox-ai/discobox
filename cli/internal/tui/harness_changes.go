package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

var harnessChangesKey = bareKey("review config")

// harnessChangeKeys is the letter each of the review's offers is pressed with.
var harnessChangeKeys = map[console.HarnessChange]string{
	console.HarnessChangeSave:    "s",
	console.HarnessChangeDiscard: "d",
}

type harnessChangesLoadedMsg struct {
	box     console.Sandbox
	changes console.HarnessChanges
	err     error
}

type harnessChangesDoneMsg struct {
	box         console.Sandbox
	candidateID string
	state       string
	err         error
}

// openHarnessChanges enters the review of a box's harness changes, from the
// registry or a gesture (the band, the explainer): the flow counts its use.
func (m *Model) openHarnessChanges(box console.Sandbox) tea.Cmd {
	m.commands.Use("review config")
	return m.reviewHarnessChanges(box)
}

// reviewHarnessChanges reads the box's harness changes again and shows them.
func (m *Model) reviewHarnessChanges(box console.Sandbox) tea.Cmd {
	m.busy = "reading harness changes…"
	return func() tea.Msg {
		changes, err := m.ds.ForServer(box.Server).HarnessChanges(m.ctx, box.ID)
		return harnessChangesLoadedMsg{box: box, changes: changes, err: err}
	}
}

func (m *Model) showHarnessChanges(msg harnessChangesLoadedMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		m.dialog = errorDialog("Harness changes", msg.err.Error())
		return nil
	}
	c := msg.changes
	var body strings.Builder
	fmt.Fprintln(&body, c.Summary())
	if c.Empty() {
		fmt.Fprintf(&body, "\n%s\n", console.HarnessChangesNothing)
	}
	fmt.Fprintf(&body, "\n%s", console.HarnessChangesSaving)
	var sections []section
	for _, f := range c.Files {
		sec := section{label: f.Label()}
		for _, text := range strings.Split(f.Diff, "\n") {
			if text == "" {
				continue
			}
			l := line{text: untab(text)}
			if style := diffStyle(text); style != nil {
				l.spans = []span{{text: l.text, style: style}}
			}
			sec.lines = append(sec.lines, l)
		}
		sections = append(sections, sec)
	}
	if len(c.Credentials) > 0 {
		sec := section{label: "credentials"}
		for _, credential := range c.Credentials {
			sec.lines = append(sec.lines, line{text: credential, bullet: true})
		}
		sections = append(sections, sec)
	}
	if len(c.Diagnostics) > 0 {
		sec := section{label: "diagnostics"}
		for _, diagnostic := range c.Diagnostics {
			sec.lines = append(sec.lines, line{text: diagnostic, bullet: true, tone: toneAlert})
		}
		sections = append(sections, sec)
	}
	// What the review offers, and when, is the core's; the letters are this
	// window's.
	var items []action
	for _, o := range c.Offers() {
		key := harnessChangeKeys[o.Change]
		items = append(items, action{key: key, press: key, label: o.Label, enabled: o.Enabled, why: o.Why})
	}
	items = append(items, action{key: "r", press: "r", label: "Refresh preview", enabled: true})
	m.dialog = actionsDialog("Harness changes — "+msg.box.Name, body.String(), items, func(key string) tea.Cmd {
		if key == "r" {
			return m.reviewHarnessChanges(msg.box)
		}
		// The closure keeps the exact ID that was displayed, even if another
		// candidate arrives while the person reads this card.
		change := console.HarnessChangeDiscard
		if key == harnessChangeKeys[console.HarnessChangeSave] {
			change = console.HarnessChangeSave
		}
		return func() tea.Msg {
			state, err := console.RunHarnessChange(m.ctx, m.ds.ForServer(msg.box.Server), msg.box.ID, change, c.CandidateID)
			return harnessChangesDoneMsg{state: state, err: err, box: msg.box, candidateID: c.CandidateID}
		}
	})
	m.dialog.sections = sections
	return nil
}

func (m *Model) harnessChangesDone(msg harnessChangesDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.dialog = errorDialog("Harness changes", msg.err.Error()+"\n\nReopen the preview to inspect current changes.")
		return nil
	}
	// The candidate is answered, but the listing goes on naming it until the
	// box next reports, which is a status poll away: the band would stand over
	// a save that worked as though it had not. It is taken down as a dismissal
	// of this candidate, so a newer one the box captures still raises it.
	if msg.candidateID != "" {
		m.dismissed.Cover(m.inbox(), console.NoticeHarnessChanges, msg.box, msg.candidateID)
		m.layout()
	}
	return tea.Batch(m.refresh(), m.report(false, "harness changes: %s", msg.state))
}

// The harness-changes band: what the workspace says about a discobox whose
// harness has settings or a login to save (ADR 26-11-02-152). There is no
// setup to run out here any more; a harness is signed in and set up inside a
// box, the way it is on anybody's own machine, and the box's agent captures
// what changed as a candidate. Nothing is shared until somebody saves it.
//
// It is the offer's green rather than the alert's red, since nothing is wrong
// and nothing is waiting; it is last in precedence for the same reason (see
// console.NoticeKind). Dismissing it holds for this candidate only: a box that captures
// a different change has a new candidate, and the band comes back for it.
// Pressing it opens the review, where Save is what takes it.

// viewHarnessChangesBanner is the workspace's line about a harness change to
// save. Its key is the review's own, behind the leader, which is what the hint
// line and the box's menu already spell.
func (m *Model) viewHarnessChangesBanner(width int) string {
	n := m.dismissed.NoticeOf(m.inbox(), console.NoticeHarnessChanges, m.currentBox())
	st := m.st
	subject := st.attentionText.Render(n.Subject)
	call := m.noticeChip(n, colOfferChip)
	return bannerRow(st, offerPalette, width, subject, call, m.leader()+" "+harnessChangesKey)
}

// The explainer (console.HarnessChangesExplainer): the core says what it
// says, which box it is about, when the project owes it
// (Session.ExplainHarnessChanges) and whether the screen is free for it
// (console.ExplainerScreen); the TUI reports its screen and puts it up. It is
// checked again on every listing, so a box that captured a change while
// something else was up is explained once that is gone.

// explainPresses are the explainer's answers by the letters that press them.
var explainPresses = map[string]string{console.ExplainReview: "r", console.ExplainLater: "n"}

// explainHarnessChangesIfOwed opens the explainer when the project owes it, a
// listed box has a change to save, and nothing else has the screen. The
// project is marked once it is answered, whichever way, Esc included.
func (m *Model) explainHarnessChangesIfOwed() tea.Cmd {
	if !m.explainHarnessChanges || !m.explainerScreen().Free() {
		return nil
	}
	var workspace *console.Sandbox
	if m.hasWorkspace() {
		box := m.currentBox()
		workspace = &box
	}
	box, ok := console.HarnessChangesToExplain(workspace, m.list.All())
	if !ok {
		return nil
	}
	m.explainHarnessChanges = false
	d := questionDialog(console.HarnessChangesExplainer(box), explainPresses, func(key string) tea.Cmd {
		if key == console.ExplainReview {
			return tea.Batch(m.openHarnessChanges(box), m.markHarnessChangesExplained())
		}
		return m.markHarnessChangesExplained()
	})
	d.onCancel = m.markHarnessChangesExplained
	m.dialog = d
	return nil
}

// explainerScreen is what has the TUI's screen, as the core reads it for the
// explainer: a dialog or the run options in front, a terminal with the keys,
// and the composer — the list's default focus — being written in.
func (m *Model) explainerScreen() console.ExplainerScreen {
	return console.ExplainerScreen{
		Covered:    m.dialog != nil || m.optionsOpen,
		Terminal:   m.inPanes(),
		Composing:  m.focus == focusPrompt,
		Draft:      m.prompt.Value() != "",
		SinceTyped: m.now().Sub(m.promptTypedAt),
	}
}

// markHarnessChangesExplained records on the project that the explainer was
// answered, behind the window, saying so when the record did not land
// (console.ExplainedHarnessChanges).
func (m *Model) markHarnessChangesExplained() tea.Cmd {
	return func() tea.Msg {
		if said := console.ExplainedHarnessChanges(m.ctx, m.ds); said != "" {
			return statusMsg{text: said, err: true}
		}
		return nil
	}
}
