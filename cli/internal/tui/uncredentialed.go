package tui

import (
	tea "charm.land/bubbletea/v2"
)

// The signed-out harness: what the window says about a discobox whose harness
// has a configure flow to collect credentials and has none bound to it — the
// harness was configured without signing in, or what it signed in with was
// deleted. It says the harness is not set up, not that anything failed, and
// that means one of two things.
//
// For a harness that needs a credential it is the refused credential's band
// (rejections.go) one step earlier. There the credential exists and an upstream
// will not take it; here there is none to take, so everything the harness
// sends goes out bare, and what comes back looks, from inside the box, exactly
// like the 401 the other band is about.
//
// For one with a free tier — opencode — nothing fails: it runs, on nothing the
// user chose. It is flagged on purpose, not as a false positive: working by
// default is why nobody would otherwise know it is meant to be configured.
//
// Either way the remedy is the same, and so is the rule: configure the harness
// out here, never sign in inside the box, where the sentinel design means a
// real credential cannot stick (ADR 0059).
//
// It shares the refusal's key and its paint for that reason: to a reader these
// are one problem — the harness has no working credential — and the band says
// which of the two it is.
//
// It is read off the listing (Sandbox.HarnessUncredentialed), which carries the
// harness's bound-secret count with every row on every server, so it costs no
// poll of its own and goes the moment a configure binds something.

// uncredentialedRemedyMsg carries the harness to configure, found on the
// discobox's own server.
type uncredentialedRemedyMsg struct {
	server  string
	harness *Harness
	name    string
	err     error
}

// uncredentialed reports whether this discobox's harness runs signed out.
//
// Not while that harness's configure flow is the screen: a bar over the setup
// telling you to run the setup is the window talking about what it is already
// doing.
func (m *Model) uncredentialed(box Sandbox) bool {
	if !box.HarnessUncredentialed || box.HarnessID == "" {
		return false
	}
	if p := m.overlay; p != nil && p.configure != nil && p.configure.harness.ID == box.HarnessID {
		return false
	}
	return true
}

// uncredentialedName is what the band calls the harness: its name, or the
// slug the row shows when it has none.
func uncredentialedName(box Sandbox) string {
	if box.HarnessName != "" {
		return box.HarnessName
	}
	return box.Harness
}

// viewUncredentialedBanner is the workspace's line about a harness with no
// credentials. It is drawn like the refusal, and for the same reasons: the chip
// does not throb, since this will still be true in a minute, and a request
// queued behind it is counted, since an agent that has no credential is the
// agent most likely to ask for one — and answering that would bind a
// credential to the box while leaving the harness itself signed out.
func (m *Model) viewUncredentialedBanner(width int) string {
	box := m.currentBox()
	st := m.st
	subject := st.attentionText.Render(uncredentialedName(box) + " has no credentials")
	if waiting := len(m.requests[box.ID]); waiting > 0 {
		subject += st.attentionHint.Render("  ·  " + plural(waiting, "request", "requests") + " waiting behind it")
	}
	// "on this machine" is the load-bearing half, as it is on the refusal: a
	// reader mid-session in the terminal under this bar reads "sign in" as the
	// harness's own /login, in the box, which cannot stick.
	call := bannerChip(st, "click to configure it on this machine", colChipLight, colAlertChip)
	return bannerRow(st, width, st.attentionMark, "⚠", subject, call, m.leader()+" "+rejectedKey, colAlertBG)
}

// openCredentialRemedy answers the band's key, which both credential bands
// print: the one on screen, so the key and a click on the same band do the
// same thing — a refusal dismissed from under the signed-out harness's band
// must not be what its key opens. With neither on screen (both dismissed, or
// something else outranking them) the key still reaches them, refusal first.
//
// It is handed the listing's row (currentBox) rather than the one the
// workspace was opened from: whether the harness has credentials is a fact
// that changes under a workspace that stays open.
func (m *Model) openCredentialRemedy(box Sandbox) tea.Cmd {
	switch m.bannerShowing() {
	case bannerRejected:
		return m.openRejectedRemedy(box)
	case bannerUncredentialed:
		return m.openUncredentialedRemedy(box)
	}
	switch {
	case m.hasRejection(box):
		return m.openRejectedRemedy(box)
	case m.uncredentialed(box):
		return m.openUncredentialedRemedy(box)
	}
	return m.report(false, "nothing wrong with this discobox's credentials")
}

// openUncredentialedRemedy opens the harness's own setup over the workspace,
// on the discobox's server — the harness is that server's, and configuring
// another server's harness of the same name is the mistake ADR 0131 §2 exists
// to prevent.
func (m *Model) openUncredentialedRemedy(box Sandbox) tea.Cmd {
	name := uncredentialedName(box)
	m.busy = "reading " + name + "…"
	ctx, ds, server, id := m.ctx, m.ds, m.serverName(box.Server), box.HarnessID
	return func() tea.Msg {
		harness, err := findHarness(ctx, ds, server, id)
		return uncredentialedRemedyMsg{server: server, harness: harness, name: name, err: err}
	}
}

// uncredentialedRemedy opens the setup that came back.
func (m *Model) uncredentialedRemedy(msg uncredentialedRemedyMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "%s: %v", msg.name, msg.err)
	}
	return m.configureHarness(msg.server, *msg.harness)
}
