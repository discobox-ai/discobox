package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Apply, in the window: the command itself in an overlay, and the question when
// it is over.
//
// The command is the CLI's own `discobox apply`, run in a pane like any other
// interaction (pane.go). Nothing here reimplements it — this file is what the
// window says about it once it is over.

// applyKey is the letter apply answers to, in the list and behind the leader
// alike, so it is a constant rather than a letter written down in places that
// have to agree.
const applyKey = "y"

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
//
// The answer is carried out in the dialog's own callback rather than sent back
// as a message. A message lands behind whatever input is already queued, and a
// second Enter — the finished report's own "done" key — would take the report
// away before the answer reached it. Carried out in place, the answer leaves
// the workspace before the next key is read, and that key acts on the list, as
// it does after the workspace's own archive key.
//
// The question is over p and goes with it (dialog.over): if the session under
// the workspace ends while it is up, there is no screen left to leave.
func (m *Model) openSuccessfulApplyDialog(p *pane) {
	items := []action{
		{key: "archive", label: "archive", detail: "put the discobox away", enabled: true},
		{key: "detach", label: "detach", detail: "leave the discobox running", enabled: true},
	}
	// The body is the heading over the summary, and the question is the answer
	// rule immediately above the rows that answer it: what was done is read
	// top-down, and what to do next is asked where the choosing happens rather
	// than a card's height above it.
	d := actionsDialog("Apply succeeded", "What this apply did:", items, func(choice string) tea.Cmd {
		return m.finishSuccessfulApply(p, choice)
	})
	if reporter, ok := p.stream.(ApplyResultReporter); ok {
		if result, ok := reporter.ApplyResult(); ok {
			d.sections = appliedSourceSections(result)
		}
	}
	d.over = p
	d.answerLabel = "what should happen to this discobox?"
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
		// A source that landed names where; one that needed nowhere says that
		// instead, in the same row, rather than an empty label — a discobox
		// can finish a successful apply with a source it never had a local
		// directory for.
		var fields []field
		switch {
		case source.Repository != "":
			fields = append(fields, field{label: "repository", value: source.Repository, tone: toneAccent})
		case source.RepositoryError != "":
			fields = append(fields, field{label: "repository", value: "none — " + source.RepositoryError, tone: toneDim})
		}
		if source.Branch != "" {
			fields = append(fields, field{label: "branch", value: source.Branch})
		}
		lines := make([]line, 0, len(source.Commits))
		for _, commit := range source.Commits {
			if commit.Commit == "" {
				continue
			}
			text := shortCommit(commit.Commit)
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

// shortCommit is how the window spells a full SHA it was handed rather than
// drawn for it: the apply report's commits, and the tip an automatic push sent.
// The listing's own commits arrive already short.
func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// finishSuccessfulApply carries out either dialog choice. Both leave the
// workspace; archive additionally changes the discobox's durable lifecycle
// state after its local terminal view has been closed. p is the report the
// question was asked over, still on screen: the question goes with it.
func (m *Model) finishSuccessfulApply(p *pane, choice string) tea.Cmd {
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
