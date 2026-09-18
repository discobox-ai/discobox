package tui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The refused credential: what the window says about a credential an upstream
// will not accept and the server cannot renew (ADR 0132).
//
// Its first job is attribution. From inside a discobox this failure looks
// exactly like Discobox failing to deliver a credential — the variable is set,
// the request went out, something on the other side said no — and the most
// useful sentence anybody can be handed is which credential was refused, and
// where. The second job is the remedy, and there are two: a credential a
// harness's configure flow made is replaced by configuring that harness again,
// and any other secret by replacing its value.
//
// The one thing the band must never read as is an invitation to sign in inside
// the discobox. That cannot work and cannot stick: the sandbox agent puts the
// delivered sentinel file back within thirty seconds (ADR 0059), and a real
// credential typed into a sandbox is what the whole sentinel design exists to
// prevent.
//
// It outranks every other band, including a credential request (see
// bannerKind), because an agent that has just taken a 401 asks for a credential
// — so the request and the refusal are one event, and the request is the half
// that cannot be usefully answered.

// rejectedKey is the band's key behind the leader: k, for the key that is not
// working. The letters this window already spends are elsewhere — g is the
// credential request, y the apply offer — and this is neither of those.
const rejectedKey = "k"

// openRejectedMsg is the leader plus that key inside a pane.
type openRejectedMsg struct{}

// secretRejectionsLoadedMsg carries the refused credentials read on the poll.
type secretRejectionsLoadedMsg struct {
	rejections []SecretRejection
	err        error
}

// rejectionRemedyMsg carries what a press needs to act: the harness to
// configure again, or the secret to replace.
type rejectionRemedyMsg struct {
	rejection SecretRejection
	harness   *Harness
	secret    *Secret
	err       error
}

// loadSecretRejections re-reads the refused credentials, one read at a time —
// the rule every polled listing here follows, so a server that has stopped
// answering collects one request rather than one a tick.
func (m *Model) loadSecretRejections() tea.Cmd {
	if !m.alertPoll.start(m.now()) {
		return nil
	}
	return func() tea.Msg {
		rejections, err := m.ds.SecretRejections(m.ctx)
		return secretRejectionsLoadedMsg{rejections: rejections, err: err}
	}
}

// setSecretRejections keeps what came back, oldest first: the credential that
// has been failing longest is the one somebody has been living with.
func (m *Model) setSecretRejections(rejections []SecretRejection) {
	sort.SliceStable(rejections, func(i, j int) bool { return rejections[i].FirstSeen.Before(rejections[j].FirstSeen) })
	had := m.bannerCost()
	m.rejections = rejections
	// The secrets screen says which credential does not work from the same
	// read: one poll answers the band and the rows, the way the credential
	// inbox already does.
	m.secrets.setRefused(rejections)
	// The band takes a row from the panes rather than adding one to the frame,
	// so a credential dying — or being replaced — resizes them.
	if m.bannerCost() != had {
		m.layout()
	}
}

// hasRejection reports whether this discobox is affected by one.
func (m *Model) hasRejection(box Sandbox) bool {
	_, ok := m.rejectionFor(box)
	return ok
}

// rejectionFor is the refused credential this discobox is affected by, if any.
//
// A harness's credential is refused for every discobox running that harness at
// once, so it matches by harness rather than by where it happened to be seen.
// Anything else matches the discobox that ran into it: a project secret that
// failed in one box says nothing about a box that has never used it.
func (m *Model) rejectionFor(box Sandbox) (SecretRejection, bool) {
	for _, rejection := range m.rejections {
		// A credential belongs to one server, and IDs are only unique within
		// one: a box on another server is running something else entirely.
		if m.serverName(rejection.Server) != m.serverName(box.Server) {
			continue
		}
		if rejection.harnessOwned() && box.HarnessID != "" && rejection.HarnessConfigID == box.HarnessID {
			return rejection, true
		}
		if !rejection.harnessOwned() && rejection.SandboxID == box.ID && box.ID != "" {
			return rejection, true
		}
	}
	return SecretRejection{}, false
}

// viewRejectedBanner is the workspace's line about a credential that does not
// work. Empty when this discobox has none.
//
// The chip does not throb, where the credential request's does. That animation
// is for an agent stopped mid-sentence with a person in the loop; this is a
// credential that has been dead for a while and will still be dead in a minute,
// and a second moving bar would spend the attention the first one is for.
func (m *Model) viewRejectedBanner(width int) string {
	rejection, ok := m.rejectionFor(m.paneBox)
	if !ok {
		return ""
	}
	st := m.st
	subject := st.attentionText.Render(rejectionHeadline(rejection))
	if rejection.Host != "" {
		subject += st.attentionHint.Render("  ·  ") + st.attentionText.Render(rejection.Host)
	}
	// A request waiting behind this one is usually the same event: the agent
	// took the 401 and asked for a credential. Saying so is what stops the
	// queued request being a mystery — and stops it being answered, which
	// would leave the dead credential bound to the harness and the new one
	// belonging to nobody. It is last in the sentence because it is the first
	// thing a narrow window should drop.
	if waiting := len(m.requests[m.paneBox.ID]); waiting > 0 {
		subject += st.attentionHint.Render("  ·  " + plural(waiting, "request", "requests") + " waiting behind it")
	}
	call := bannerChip(st, rejectionCall(rejection), colChipLight, colAlertChip)
	return bannerRow(st, width, st.attentionMark, "⚠", subject, call, m.leader()+" "+rejectedKey, colAlertBG)
}

// rejectionHeadline is the bar's sentence: what was refused, and by whom.
//
// It says "refused" rather than "expired" or "invalid" because refused is what
// is actually known — the upstream said no to a credential this side could not
// renew — and because the two words a reader might otherwise supply for
// themselves, *your login inside this box*, are the ones that send them
// somewhere nothing can be fixed.
func rejectionHeadline(rejection SecretRejection) string {
	if rejection.harnessOwned() {
		return rejection.HarnessConfigName + " sign-in refused"
	}
	return rejection.name() + " refused"
}

// rejectionCall is what pressing the band does, in the words of the gesture
// rather than of a button, so the sentence still says it with no color to draw
// a chip with.
//
// It names *where*, and that is the load-bearing half. This bar is drawn across
// a screen that is otherwise the discobox's own terminal, to a reader who is
// mid-session in it, and "sign in again" on its own reads as the thing they
// were already about to try: the harness's own `/login`, in the box, which
// cannot work and cannot stick (ADR 0059, ADR 0132 §5). "on this machine" is
// the whole difference between sending somebody to the fix and sending them
// round the loop again.
func rejectionCall(rejection SecretRejection) string {
	if rejection.harnessOwned() {
		return "click to sign in on this machine"
	}
	return "click to replace it on this machine"
}

// openRejectedRemedy answers a press on the band: the harness's own setup for a
// credential its configure flow made, and the secret's card for anything else.
//
// Both open over the workspace rather than taking you to another screen, and
// both end back on the discobox you were watching — which is the whole shape of
// this: the fix is out here, it takes a minute, and the session you were in the
// middle of is still there when it is done.
func (m *Model) openRejectedRemedy(box Sandbox) tea.Cmd {
	rejection, ok := m.rejectionFor(box)
	if !ok {
		return m.report(false, "nothing refused on this discobox")
	}
	m.busy = "reading " + rejection.name() + "…"
	// The credential's own server, not the one the header names: the fix goes
	// where the credential lives (ADR 0131 §2).
	ctx, ds, server := m.ctx, m.ds, m.serverName(rejection.Server)
	return func() tea.Msg {
		if rejection.harnessOwned() {
			harnesses, err := ds.Harnesses(ctx, server)
			if err != nil {
				return rejectionRemedyMsg{rejection: rejection, err: err}
			}
			for _, harness := range harnesses {
				if harness.ID == rejection.HarnessConfigID {
					return rejectionRemedyMsg{rejection: rejection, harness: &harness}
				}
			}
			return rejectionRemedyMsg{rejection: rejection, err: errNoHarness}
		}
		secrets, err := ds.Secrets(ctx, server)
		if err != nil {
			return rejectionRemedyMsg{rejection: rejection, err: err}
		}
		for _, secret := range secrets {
			if secret.ID == rejection.SecretID {
				return rejectionRemedyMsg{rejection: rejection, secret: &secret}
			}
		}
		return rejectionRemedyMsg{rejection: rejection, err: errNoSecret}
	}
}

// rejectionRemedy opens whichever remedy came back.
func (m *Model) rejectionRemedy(msg rejectionRemedyMsg) tea.Cmd {
	m.busy = ""
	switch {
	case msg.err != nil:
		return m.report(true, "%s: %v", msg.rejection.name(), msg.err)
	case msg.harness != nil:
		return m.configureHarness(m.serverName(msg.rejection.Server), *msg.harness)
	case msg.secret != nil:
		return m.editSecretForm(m.serverName(msg.rejection.Server), *msg.secret)
	}
	return nil
}

type rejectionError string

func (e rejectionError) Error() string { return string(e) }

const (
	// The two ways the remedy can have nothing to open: a harness or a secret
	// the listing no longer holds. Both mean the same thing to a reader — the
	// thing that was refused is gone — and both are reported rather than
	// swallowed, since the band is still on screen saying otherwise.
	errNoHarness = rejectionError("the harness that owns it is gone")
	errNoSecret  = rejectionError("the credential is gone")
)

// rejectionSummary is one line about a refused credential for the secrets
// screen's rows, where the reason is worth more room than a band has.
func rejectionSummary(rejection SecretRejection) string {
	parts := []string{"refused at " + rejection.Host}
	switch rejection.Reason {
	case "unrefreshable":
		parts = append(parts, "nothing to renew")
	case "refresh-failed":
		parts = append(parts, "renewal refused")
	case "rejected-after-refresh":
		parts = append(parts, "refused again after renewing")
	}
	return strings.Join(parts, " · ")
}
