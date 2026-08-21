package tui

import (
	tea "charm.land/bubbletea/v2"
)

// A prompt is often the most valuable thing on the screen: several lines
// thought about and not yet run. The window keeps it across launches so that
// closing the window — on purpose, by reflex, or because the terminal went
// away — is not how you lose one.
//
// The draft is kept per folder, and the folder is the session's own directory:
// the project you are standing in, which is the one Enter would create in. A
// prompt written in one checkout has no business coming back in another.
//
// Where it is kept is the DataSource's business (SaveDraft), the way everything
// outside this window is. The window's part is when: on the listing's clock
// while it is open, and on the way out.

// restoreDraft puts back the prompt the last window left in this folder.
//
// Only into an empty composer. The session lands a moment after the window is
// up and the cursor is in the field from the first frame, so anything already
// typed is what is being written now, and what is being written now wins.
func (m *Model) restoreDraft(draft string) tea.Cmd {
	m.draft = draft
	if draft == "" || m.prompt.Value() != "" {
		return nil
	}
	m.prompt.SetValue(draft)
	m.promptEnd()
	// There are words in the field, so the glint over the placeholder has
	// nothing left to play on.
	m.stopShimmer()
	m.layout()
	return m.report(false, "the prompt you left here is back")
}

// saveDraft writes the prompt if it has moved since the store last had it, and
// returns nil when it has not, so an idle window writes nothing at all.
func (m *Model) saveDraft() tea.Cmd {
	folder, prompt, ok := m.draftToSave()
	if !ok {
		return nil
	}
	ds, ctx := m.ds, m.ctx
	return func() tea.Msg {
		if err := ds.SaveDraft(ctx, folder, prompt); err != nil {
			return statusMsg{text: "cannot save the prompt: " + err.Error(), err: true}
		}
		return nil
	}
}

// saveDraftNow writes it on the way out, and is the one place the window
// reaches the store from the update loop rather than from a command: a command
// batched with tea.Quit races the runtime shutting down, and a prompt lost on
// the way out is the whole of what this exists to prevent.
//
// Nothing is reported. There is no frame left to report it on.
func (m *Model) saveDraftNow() {
	folder, prompt, ok := m.draftToSave()
	if !ok {
		return
	}
	_ = m.ds.SaveDraft(m.ctx, folder, prompt)
}

// draftToSave is what a write would carry, and whether there is one to make.
//
// Marking it saved before it lands is deliberate: a write that fails is
// reported and left alone until the prompt changes again, because the
// alternative is a window that says the same thing about the same broken disk
// every five seconds.
func (m *Model) draftToSave() (folder, prompt string, ok bool) {
	folder, prompt = m.session.Directory, m.prompt.Value()
	if folder == "" || prompt == m.draft {
		return "", "", false
	}
	m.draft = prompt
	return folder, prompt, true
}

// closeWindow ends the program, with whatever is in the composer saved first.
// Every key that closes the window goes through here.
func (m *Model) closeWindow() tea.Cmd {
	m.saveDraftNow()
	m.quit = true
	return tea.Quit
}
