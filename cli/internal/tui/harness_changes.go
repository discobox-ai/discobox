package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

var harnessChangesKey = bareKey("review config")

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
	fmt.Fprintf(&body, "State: %s · revision %d\n", c.State, c.Revision)
	if len(c.Files)+len(c.Credentials) == 0 {
		body.WriteString("\nNo saveable settings or credentials.\n")
	}
	body.WriteString("\nSave updates shared harness defaults and credentials. Discard leaves local files unchanged.")
	var sections []section
	for _, f := range c.Files {
		sec := section{label: f.Action + " " + f.Path}
		if f.Shadowed {
			sec.label += " — shadowed by your override, which boxes keep getting"
		}
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
	saveable := c.CandidateID != "" && ((c.State == "pending" && len(c.Files)+len(c.Credentials) > 0) || c.State == "saved-handoff-pending")
	items := []action{
		{key: "s", press: "s", label: "Save reviewed changes", enabled: saveable, why: "no saveable candidate"},
		{key: "d", press: "d", label: "Discard candidate", enabled: c.CandidateID != "" && c.State == "pending", why: "no pending candidate"},
		{key: "r", press: "r", label: "Refresh preview", enabled: true},
	}
	m.dialog = actionsDialog("Harness changes — "+msg.box.Name, body.String(), items, func(key string) tea.Cmd {
		if key == "r" {
			return m.reviewHarnessChanges(msg.box)
		}
		// The closure keeps the exact ID that was displayed, even if another
		// candidate arrives while the person reads this card.
		return func() tea.Msg {
			var state string
			var err error
			if key == "s" {
				state, err = m.ds.ForServer(msg.box.Server).SaveHarnessChanges(m.ctx, msg.box.ID, c.CandidateID)
			} else {
				state, err = m.ds.ForServer(msg.box.Server).DiscardHarnessChanges(m.ctx, msg.box.ID, c.CandidateID)
			}
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
		if m.dismissed == nil {
			m.dismissed = map[dismissal][]string{}
		}
		d := m.dismissalFor(bannerHarnessChanges, msg.box)
		m.dismissed[d] = append(m.dismissed[d], msg.candidateID)
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
// bannerKind). Dismissing it holds for this candidate only: a box that captures
// a different change has a new candidate, and the band comes back for it.
// Pressing it opens the review, where Save is what takes it.

// harnessChangesName is what the band calls the box's harness: its name, or
// the slug the row shows when it has none.
func harnessChangesName(box console.Sandbox) string {
	if box.HarnessName != "" {
		return box.HarnessName
	}
	if box.Harness != "" {
		return box.Harness
	}
	return "the harness"
}

// viewHarnessChangesBanner is the workspace's line about a harness change to
// save. Its key is the review's own, behind the leader, which is what the hint
// line and the box's menu already spell.
func (m *Model) viewHarnessChangesBanner(width int) string {
	box := m.currentBox()
	st := m.st
	subject := st.attentionText.Render(harnessChangesName(box) + " settings or login changed in this box")
	call := bannerChip(st, "click to review and save", colOfferChip)
	return bannerRow(st, offerPalette, width, subject, call, m.leader()+" "+harnessChangesKey)
}

// The explainer: the first time a project's console sees a discobox with a
// harness change to save, it says what saving one does before anybody is asked
// to. The review card says what changed; this says what Save means — that the
// change stops being this box's and becomes every box's on the harness, a
// login included — which is the one thing about it a person cannot see from
// the diff.
//
// Once is per project, and the project is what remembers
// (Project.harnessChangesExplained): a file on this machine would explain again
// on a second machine and never to the second person on the project. The window reads the flag when it opens
// (WithHarnessChangesUnexplained) and sets it the moment it shows the
// explainer, whichever way it is answered: explaining twice because somebody
// pressed Not now would be the explainer outstaying its one job.
//
// It waits for a screen with nothing in front of it — no dialog,
// no run options, no terminal with the keys and no prompt being written —
// and is checked again on every listing, so a box that captured a change while
// something else was up is explained once that is gone. The terminal matters most: the change is
// made by signing in there, so the explainer would otherwise come up while
// the person is still typing, and its actions are single letters.

// WithHarnessChangesUnexplained opens the window owing the explainer: the
// project has not yet explained saving a discobox's harness changes.
func WithHarnessChangesUnexplained() Option {
	return func(m *Model) { m.explainHarnessChanges = true }
}

const harnessChangesExplainTitle = "Saving harness changes"

// harnessChangesExplainBody is the explainer's text for a harness by name.
func harnessChangesExplainBody(name string) string {
	return name + " in this discobox changed its settings or signed in.\n\n" +
		"Saving makes those settings, and that login, the shared default for every discobox on " + name + " in this project: " +
		"new discoboxes start with them, and a saved login is the one all of them use.\n\n" +
		"Settings can include hooks and commands, which then run in every discobox on " + name + ": read what changed before you save it.\n\n" +
		"Nothing is shared until you save. Discarding the change, or leaving it, keeps it in this discobox only.\n\n" +
		"Whenever a box has a change to save, a green band says so and " + harnessChangesKey + " behind the leader reviews it."
}

// explainHarnessChangesIfOwed opens the explainer when the project owes it, a
// listed box has a change to save, and nothing else has the screen.
func (m *Model) explainHarnessChangesIfOwed() tea.Cmd {
	if !m.explainHarnessChanges || m.dialog != nil || m.optionsOpen || m.inPanes() || m.writingPrompt() {
		return nil
	}
	box, ok := m.harnessChangesToExplain()
	if !ok {
		return nil
	}
	m.explainHarnessChanges = false
	items := []action{
		{key: "r", press: "r", label: "Review changes", detail: "see what changed, then save or discard it", enabled: true},
		{key: "n", press: "n", label: "Not now", detail: "the band and " + harnessChangesKey + " stay to review it later", enabled: true},
	}
	m.dialog = actionsDialog(harnessChangesExplainTitle, harnessChangesExplainBody(harnessChangesName(box)), items, func(key string) tea.Cmd {
		if key == "r" {
			return m.reviewHarnessChanges(box)
		}
		return nil
	})
	return m.markHarnessChangesExplained()
}

// promptQuiet is how long the composer has to have gone untyped in before the
// explainer comes up while it has the keys.
const promptQuiet = 5 * time.Second

// writingPrompt is whether the person is writing in the composer: it has the
// keys, and holds a draft or was typed into just now. A letter typed at the
// explainer as it came up would answer it.
func (m *Model) writingPrompt() bool {
	if m.focus != focusPrompt {
		return false
	}
	return m.prompt.Value() != "" || m.now().Sub(m.promptTypedAt) < promptQuiet
}

// harnessChangesToExplain is the box the explainer is about: the one on
// screen when it has a change, else the first listed box that does.
func (m *Model) harnessChangesToExplain() (console.Sandbox, bool) {
	if m.hasWorkspace() {
		if box := m.currentBox(); box.HarnessConfigPending {
			return box, true
		}
	}
	for _, box := range m.list.All() {
		if box.HarnessConfigPending {
			return box, true
		}
	}
	return console.Sandbox{}, false
}

// markHarnessChangesExplained tells the server the explainer has been shown,
// behind the dialog: the explainer has been read whether
// or not the write lands, and losing it costs one repeated explanation.
func (m *Model) markHarnessChangesExplained() tea.Cmd {
	return func() tea.Msg {
		if err := m.ds.MarkHarnessChangesExplained(m.ctx); err != nil {
			return statusMsg{text: "could not save that harness changes were explained: " + err.Error(), err: true}
		}
		return nil
	}
}
