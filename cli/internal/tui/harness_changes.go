package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
)

const harnessChangesKey = "H"

// HarnessChanges contains only the server's safe preview, never raw credentials.
type HarnessChanges struct {
	CandidateID string
	State       string
	Revision    int64
	Files       []HarnessChangedFile
	Credentials []string
	Diagnostics []string
}

// HarnessChangedFile is one file a candidate adds, updates or removes, shown
// as what changed in it rather than what it now holds.
type HarnessChangedFile struct {
	Path string
	// Action is add, update or remove.
	Action string
	// Diff is the change's unified diff hunks against the file it replaces.
	Diff string
	// Shadowed is a change at a path a person overrode: a save keeps it
	// beneath the override, and boxes go on being given the person's file
	// (ADR 26-11-04-711 §2).
	Shadowed bool
}

type harnessChangesLoadedMsg struct {
	box     Sandbox
	changes HarnessChanges
	err     error
	// unasked is a review the explainer opened: it came up over a terminal
	// the person may still be typing into, so its card holds its keys too.
	unasked bool
}

type harnessChangesDoneMsg struct {
	box         Sandbox
	candidateID string
	state       string
	err         error
}

// reviewHarnessChanges reads a box's harness changes into the review card.
// unasked is a review reached from the explainer, which came up unasked: the
// card then holds its keys until the keyboard is quiet, as the explainer did,
// so typing that answered the explainer cannot go on to save.
func (m *Model) reviewHarnessChanges(box Sandbox, unasked bool) tea.Cmd {
	m.busy = "reading harness changes…"
	return func() tea.Msg {
		changes, err := m.ds.ForServer(box.Server).HarnessChanges(m.ctx, box.ID)
		return harnessChangesLoadedMsg{box: box, changes: changes, err: err, unasked: unasked}
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
	// A review reached from the explainer, which came up unasked over typing,
	// commits on no letter: a hold says only that the keyboard paused, not
	// that the card was read. Save and Discard are chosen with the arrow
	// keys there (not j and k: arrowsOnly), Enter lands on Refresh, and
	// Refresh keeps it that way.
	commitPress := func(key string) string {
		if msg.unasked {
			return ""
		}
		return key
	}
	items := []action{
		{key: "s", press: commitPress("s"), label: "Save reviewed changes", enabled: saveable, why: "no saveable candidate"},
		{key: "d", press: commitPress("d"), label: "Discard candidate", enabled: c.CandidateID != "" && c.State == "pending", why: "no pending candidate"},
		{key: "r", press: "r", label: "Refresh preview", enabled: true},
	}
	m.dialog = actionsDialog("Harness changes — "+msg.box.Name, body.String(), items, func(key string) tea.Cmd {
		if key == "r" {
			return m.reviewHarnessChanges(msg.box, msg.unasked)
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
	if msg.unasked {
		m.dialog.holdUntilQuiet(m.now())
		m.dialog.arrowsOnly = true
		m.dialog.cursor = slices.IndexFunc(items, func(it action) bool { return it.key == "r" })
	}
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
func harnessChangesName(box Sandbox) string {
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
// to. Signing in inside a box is how a harness gets set up, and nothing in the
// box says the login dies with it unless it is saved; this is where a person
// learns that it need not. The review card says what changed; this says what
// Save means — that the change stops being this box's and becomes every box's
// on the harness, a login included — which is the one thing about it a person
// cannot see from the diff.
//
// Once is per project, and the project is what remembers
// (Project.harnessChangesExplained): a file on this machine would explain again
// on a second machine and never to the second person on the project. The
// window reads the flag when it opens (WithHarnessChangesUnexplained) and sets
// it the moment it shows the explainer, whichever way it is answered:
// explaining twice because somebody pressed Not now would be the explainer
// outstaying its one job.
//
// It comes up on the first listing that reports the change, over whatever has
// the keys — usually the very terminal the person just signed in at, in the
// bare attach window — because that is the moment the explanation is for.
// Only another dialog or the run options hold it, and it is checked again on
// every listing so it follows them. Its answers are single letters, so it
// takes no key until the keyboard has been quiet (holdUntilQuiet): what the
// person was typing as it came up was meant for the terminal.

// WithHarnessChangesUnexplained opens the window owing the explainer: the
// project has not yet explained saving a discobox's harness changes.
func WithHarnessChangesUnexplained() Option {
	return func(m *Model) { m.explainHarnessChanges = true }
}

const harnessChangesExplainTitle = "Save this harness setup to discobox?"

// harnessChangesExplainBody is the explainer's text for a harness by name.
func harnessChangesExplainBody(name string) string {
	return name + " in this discobox changed its settings or signed in.\n\n" +
		"Nothing is shared until you save: it lives only in this discobox. Saved to discobox, it becomes the shared default every new launch of " + name + " in this project starts with — " +
		"the same settings, and the same login, so nobody has to sign in again.\n\n" +
		"Settings can include hooks and commands, which then run in every discobox on " + name + ": read what changed before you save it.\n\n" +
		"Discarding the change, or leaving it, keeps it in this discobox only.\n\n" +
		"Whenever a box has a change to save, a green band says so and " + harnessChangesKey + " behind the leader reviews it."
}

// explainHarnessChangesIfOwed opens the explainer when the project owes it, a
// listed box has a change to save, and no other dialog is up.
func (m *Model) explainHarnessChangesIfOwed() tea.Cmd {
	if !m.explainHarnessChanges || m.dialog != nil || m.optionsOpen {
		return nil
	}
	box, ok := m.harnessChangesToExplain()
	if !ok {
		return nil
	}
	m.explainHarnessChanges = false
	items := []action{
		{key: "r", press: "r", label: "Review and save", detail: "see what changed, then save or discard it", enabled: true},
		{key: "n", press: "n", label: "Not now", detail: "the band and " + harnessChangesKey + " stay to review it later", enabled: true},
	}
	m.dialog = actionsDialog(harnessChangesExplainTitle, harnessChangesExplainBody(harnessChangesName(box)), items, func(key string) tea.Cmd {
		if key == "r" {
			return m.reviewHarnessChanges(box, true)
		}
		return nil
	}).holdUntilQuiet(m.now())
	return m.markHarnessChangesExplained()
}

// harnessChangesToExplain is the box the explainer is about: the one on
// screen when it has a change, else the first listed box that does.
func (m *Model) harnessChangesToExplain() (Sandbox, bool) {
	if m.hasWorkspace() {
		if box := m.currentBox(); box.HarnessConfigPending {
			return box, true
		}
	}
	for _, box := range m.list.all {
		if box.HarnessConfigPending {
			return box, true
		}
	}
	return Sandbox{}, false
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
