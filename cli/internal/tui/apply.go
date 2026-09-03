package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Apply, in the window: the offer while there is committed work no apply has
// landed, the command itself in an overlay, and the question when it is over.
//
// The command is the CLI's own `discobox apply`, run in a pane like any other
// interaction (pane.go). Nothing here reimplements it — this file is what the
// window says about it before and after.

// applyKey is the letter apply answers to, in the list and behind the leader
// alike. The ready band names it, so it is a constant rather than a letter
// written down in three places that have to agree.
const applyKey = "y"

// applyReady reports whether the discobox the workspace is showing is holding
// committed work that no apply has landed — the state the list spells `ready`,
// and the one moment when there is something to bring home and no reason to
// wait for it.
//
// It reads the listing through currentBox rather than the snapshot the
// workspace was opened on, the same as the header does: a bar offering to apply
// what has already been applied is worse than no bar.
func (m *Model) applyReady() bool {
	if m.overlay != nil && m.overlay.action == InteractApply {
		// The apply is the screen. A bar over the top of it saying an apply is
		// available is the window talking about what it is already doing.
		return false
	}
	return m.currentBox().ahead()
}

// viewApplyBanner is the workspace's line about work that is ready to come
// home: the same bar the credential request gets, in green rather than red,
// because this one is an offer rather than a person waiting on you.
//
// The mark is the list's own `⇡` for the same state, so the bar and the row are
// plainly about the same thing.
func (m *Model) viewApplyBanner(width int) string {
	st := m.st
	subject := st.attentionText.Render("ready to apply")
	if detail := applyReadyDetail(m.currentBox()); detail != "" {
		subject += st.attentionHint.Render("  ·  ") + st.attentionText.Render(detail)
	}
	return bannerRow(st, width, st.readyMark, "⇡", subject, m.leader()+" "+applyKey, "apply", colReadyBG)
}

// applyReadyDetail is how much is waiting, in the list's own spelling of a
// diffstat. It is the diffstat rather than a commit count because the diffstat
// is what the listing carries; a count of commits would cost a request per
// draw for a number that says less about how much is at stake.
func applyReadyDetail(box Sandbox) string {
	if !box.hasDiff() {
		return ""
	}
	// The header a row above spells a diffstat with a real minus sign
	// (diffText); the band says the same numbers the same way, plus the file
	// count the header has no room for.
	return fmt.Sprintf("+%d −%d in %s", box.Diff.Added, box.Diff.Deleted, plural(box.Diff.Files, "file", "files"))
}

// confirmApply is the band's click: the same command the key runs, behind the
// one question a click has to answer that a keystroke does not.
//
// The key is deliberate — it is a leader chord, typed by somebody who read the
// bar that names it — and it runs straight into the apply, which is what the
// same key does from the list. A click is a press on a bar that happens to be
// the width of the window and sits a row under the header, where a mistimed
// click on a tab lands: the gesture is cheap, so the window says what it is
// about to do to a repository outside the discobox before it does it.
func (m *Model) confirmApply() tea.Cmd {
	box := m.currentBox()
	fields := []field{{label: "from", value: box.Name, tone: toneAccent}}
	if pos := box.base(); pos != "" {
		fields = append(fields, field{label: "at", value: pos})
	}
	if m.session.Directory != "" {
		fields = append(fields, field{label: "into", value: m.session.Directory, tone: toneAccent})
	}
	if detail := applyReadyDetail(box); detail != "" {
		fields = append(fields, field{label: "changes", value: detail})
	}
	d := confirmDialog("Apply", "", func(string) tea.Cmd {
		// Through the list's own dispatcher, so the click and the key reach
		// the apply the same way: same enabled checks, same overlay, same
		// report. It re-reads the box because the listing ticks under the
		// dialog while the question is up.
		return m.actOn(applyKey, []Sandbox{m.currentBox()})
	})
	d.sections = []section{{
		label:  "applying",
		fields: fields,
		lines: []line{
			{text: "the discobox's commits are fetched and cherry-picked onto the local working tree", bullet: true},
			{text: "uncommitted changes stay in the discobox — only commits move, and only this way", bullet: true},
			{text: "if one does not apply cleanly nothing local changes; the report says how to finish by hand", bullet: true},
		},
	}}
	d.answerLabel = "apply now?"
	d.footer = "the report opens over the workspace; " + m.leader() + " " + applyKey + " starts it without this question"
	m.dialog = d
	return nil
}

// successfulApply is the finished apply report that offers what usually comes
// next: put the box away, leave it running and detach, or return to it. A failed
// apply stays an ordinary readable report; cleanup must never be the prominent
// next action while its error still needs attention.
func (m *Model) successfulApply(p *pane) bool {
	if p != m.overlay || p.action != InteractApply || !p.exited || p.failed {
		return false
	}
	code, done := exitCode(p.stream)
	return done && code == 0
}

// openSuccessfulApplyDialog asks what to do with a box whose work is safely
// back on the host. Archive leads because it is the usual cleanup; Escape
// dismisses only the question and leaves the apply report on screen.
func (m *Model) openSuccessfulApplyDialog() {
	items := []action{
		{key: "archive", label: "archive", detail: "put the discobox away", enabled: true},
		{key: "detach", label: "detach", detail: "leave the discobox running", enabled: true},
	}
	d := actionsDialog("Apply succeeded", "What should happen to this discobox?", items, func(choice string) tea.Cmd {
		return func() tea.Msg { return applyFinishedChoiceMsg{choice: choice} }
	})
	if reporter, ok := m.overlay.stream.(ApplyResultReporter); ok {
		if result, ok := reporter.ApplyResult(); ok {
			d.sections = appliedSourceSections(result)
		}
	}
	d.footer = "Either choice detaches from the workspace."
	d.keys = []hint{pressing("Enter chooses", "enter"), pressing("Esc returns to the apply result", "esc")}
	m.dialog = d
}

// appliedSourceSections name every destination and the local commits created
// there. A source that needed nothing still appears, so a multi-source apply's
// success dialog accounts for the whole run rather than only the sources that
// changed.
func appliedSourceSections(result ApplyResult) []section {
	sections := make([]section, 0, len(result.Sources))
	for _, source := range result.Sources {
		fields := []field{{label: "repository", value: source.Repository, tone: toneAccent}}
		if source.Branch != "" {
			fields = append(fields, field{label: "branch", value: source.Branch})
		}
		lines := make([]line, 0, len(source.Commits))
		for _, commit := range source.Commits {
			if commit.Commit == "" {
				continue
			}
			text := shortAppliedCommit(commit.Commit)
			if commit.Subject != "" {
				text += "  " + commit.Subject
			}
			lines = append(lines, line{text: text, tone: toneOK, bullet: true})
		}
		if len(lines) == 0 {
			text := "no local commit mapping available — see the apply report"
			if source.Status == "up-to-date" {
				text = "no commits created — already up to date"
			}
			lines = append(lines, line{text: text, tone: toneDim})
		}
		sections = append(sections, section{label: source.Slug, fields: fields, lines: lines})
	}
	return sections
}

func shortAppliedCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// finishSuccessfulApply carries out either dialog choice. Both leave the
// workspace; archive additionally changes the discobox's durable lifecycle
// state after its local terminal view has been closed.
func (m *Model) finishSuccessfulApply(choice string) tea.Cmd {
	p := m.overlay
	if !m.successfulApply(p) {
		return nil
	}
	id := p.sandbox.ID
	hadWorkspace := m.terminals.len() > 0
	m.closeWorkspace()
	m.layout()
	if choice == "archive" {
		return m.runVerb(VerbArchive, []string{id})
	}
	if m.attach != nil && hadWorkspace {
		return m.exit(nil)
	}
	if hadWorkspace {
		return tea.Batch(m.refresh(), status("detached — the discobox is still running"))
	}
	return m.refresh()
}
