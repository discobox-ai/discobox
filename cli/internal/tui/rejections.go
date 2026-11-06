package tui

import (
	"slices"
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
// where. The second job is the remedy, and there are two. A harness's shared
// login is replaced the way it was made (ADR 26-11-02-152): sign in again with
// the harness's own login inside a discobox, and save that box's harness
// change, which replaces the refused login in place for every box on the
// harness. Any other secret is replaced by giving it a new value out here.
//
// The remedy names a fresh discobox, not the one the refusal showed in: a box
// can provoke a refusal, and a login typed into it is readable there. And the
// refusal is never taken down by a change the box reports (bannerWanted).
//
// It outranks every other band, including a credential request (see
// bannerKind), because an agent that has just taken a 401 asks for a credential
// — so the request and the refusal are one event, and the request is the half
// that cannot be usefully answered.

// rejectedKey is the band's key behind the leader: K, for the key that is not
// working — capital, because k and j move along the workspace tree. The
// letters this window already spends are elsewhere — g is the credential
// request.
const rejectedKey = "K"

// openRejectedMsg is the leader plus that key inside a pane.
type openRejectedMsg struct{}

// secretRejectionsLoadedMsg carries the refused credentials read on the poll.
type secretRejectionsLoadedMsg struct {
	rejections []SecretRejection
	err        error
}

// rejectionRemedyMsg carries what a press needs to act: the secret to
// replace. A harness's login needs nothing read to say how to replace it.
type rejectionRemedyMsg struct {
	rejection SecretRejection
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
	m.pruneDismissed()
	// The band takes a row from the panes rather than adding one to the frame,
	// so a credential dying — or being replaced — resizes them.
	if m.bannerCost() != had {
		m.layout()
	}
}

// hasRejection reports whether this discobox is affected by one.
func (m *Model) hasRejection(box Sandbox) bool {
	return len(m.rejectionsFor(box)) > 0
}

// shownRejection is the refusal the band is about: the oldest one a dismissal
// has not covered. A band back up for a second refusal is about that one — its
// name, its host, its remedy — and not the first, which somebody already
// dismissed; dismissing the band again then dismisses what it showed. With
// nothing dismissed, or everything (the key reaches a band that is not drawn),
// it is the oldest.
func (m *Model) shownRejection(box Sandbox) (SecretRejection, bool) {
	all := m.rejectionsFor(box)
	if len(all) == 0 {
		return SecretRejection{}, false
	}
	dismissed := m.dismissed[m.dismissalFor(bannerRejected, box)]
	for _, rejection := range all {
		if !slices.Contains(dismissed, rejection.occurrence()) {
			return rejection, true
		}
	}
	return all[0], true
}

// rejectionsFor is every refused credential this discobox is affected by,
// oldest first. Which of them the band is about is shownRejection's to say.
//
// A harness's credential is refused for every discobox running that harness at
// once, so it matches by harness rather than by where it happened to be seen.
// Anything else matches the discobox that ran into it: a project secret that
// failed in one box says nothing about a box that has never used it.
func (m *Model) rejectionsFor(box Sandbox) []SecretRejection {
	var out []SecretRejection
	for _, rejection := range m.rejections {
		// A credential belongs to one server, and IDs are only unique within
		// one: a box on another server is running something else entirely.
		if m.session.serverName(rejection.Server) != m.session.serverName(box.Server) {
			continue
		}
		if rejection.harnessOwned() && box.HarnessID != "" && rejection.HarnessConfigID == box.HarnessID ||
			!rejection.harnessOwned() && rejection.SandboxID == box.ID && box.ID != "" {
			out = append(out, rejection)
		}
	}
	return out
}

// viewRejectedBanner is the workspace's line about a credential that does not
// work. Empty when this discobox has none.
//
// The chip does not throb, where the credential request's does. That animation
// is for an agent stopped mid-sentence with a person in the loop; this is a
// credential that has been dead for a while and will still be dead in a minute,
// and a second moving bar would spend the attention the first one is for.
func (m *Model) viewRejectedBanner(width int) string {
	// The listing's row, as bannerShowing reads it: the band that is chosen
	// and the band that is drawn must be about the same row, or the panes give
	// up two rows to a band that draws nothing.
	box := m.currentBox()
	rejection, ok := m.shownRejection(box)
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
	if waiting := len(m.pendingFor(box)); waiting > 0 {
		subject += st.attentionHint.Render("  ·  " + plural(waiting, "request", "requests") + " waiting behind it")
	}
	call := bannerChip(st, rejectionCall(rejection), colAlertChip)
	return bannerRow(st, alertPalette, width, subject, call, m.leader()+" "+rejectedKey)
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
// A harness's login is replaced in the box, so its call says how; any other
// secret is replaced out here, and its call says that.
func rejectionCall(rejection SecretRejection) string {
	if rejection.harnessOwned() {
		return "click to sign in again"
	}
	return "click to replace it on this machine"
}

// openRejectedRemedy answers a press on the band: how to sign in again for a
// harness's shared login, and the secret's card for anything else.
//
// Both open over the workspace rather than taking you to another screen, and
// both end back on the discobox you were watching: the session you were in the
// middle of is still there when it is done.
func (m *Model) openRejectedRemedy(box Sandbox) tea.Cmd {
	rejection, ok := m.shownRejection(box)
	if !ok {
		return m.report(false, "nothing refused on this discobox")
	}
	if rejection.harnessOwned() {
		m.dialog = signInAgainDialog(rejection)
		return nil
	}
	m.busy = "reading " + rejection.name() + "…"
	// The credential's own server, not the one the header names: the fix goes
	// where the credential lives (ADR 0131 §2).
	ctx, ds, server := m.ctx, m.ds, m.session.serverName(rejection.Server)
	return func() tea.Msg {
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

// signInAgainDialog says how a harness's refused shared login is replaced:
// in a discobox, by the harness's own sign-in, and then saved. It is a message
// rather than an action because the action is the person's, in the terminal
// behind it.
func signInAgainDialog(rejection SecretRejection) *dialog {
	name := rejection.HarnessConfigName
	if name == "" {
		name = "the harness"
	}
	body := name + "'s shared login was refused, and could not be renewed. Every discobox on " + name + " uses it.\n\n" +
		"Sign in again in a new discobox on " + name + ", the way " + name + " signs in anywhere — its own login command, in its terminal. " +
		"A login typed in a box can be read by the code running there, so use one whose code you trust rather than the box this refusal showed in.\n\n" +
		"The new login shows up as a harness change on that box: save it, and it replaces the refused login for every discobox on " + name + ". " +
		"Saving also shares the box's changed settings files, which can run commands in every discobox on " + name + ": read the review before you save."
	d := &dialog{kind: dlgMessage, title: name + " sign-in refused", body: body}
	return d
}

// rejectionRemedy opens whichever remedy came back.
func (m *Model) rejectionRemedy(msg rejectionRemedyMsg) tea.Cmd {
	m.busy = ""
	switch {
	case msg.err != nil:
		return m.report(true, "%s: %v", msg.rejection.name(), msg.err)
	case msg.secret != nil:
		return m.editSecretForm(m.session.serverName(msg.rejection.Server), *msg.secret)
	}
	return nil
}

type rejectionError string

func (e rejectionError) Error() string { return string(e) }

const (
	// The remedy has nothing to open when the listing no longer holds the
	// secret: the thing that was refused is gone, which is reported rather
	// than swallowed, since the band is still on screen saying otherwise.
	errNoSecret = rejectionError("the credential is gone")
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
