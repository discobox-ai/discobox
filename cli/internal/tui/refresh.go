package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"

	tea "charm.land/bubbletea/v2"
)

// A refresh request asks for a new value of a token the project holds, because
// its value went stale or an upstream refused it (ADR 26-09-25-122). It sits in
// the inbox with every other ask, and is answered by writing a value rather
// than by approving: running the command the token suggests, here, or typing
// one in.
//
// The window is what offers to run the command, because it is where a person
// is — the command runs on their machine, with their login, and they see it
// before it runs. "Run for this session" remembers the token and the exact
// command for as long as this window runs, and answers that pair's next asks
// without a prompt; an edited command is a different pair and asks again.

// renewalAnsweredMsg reports a renewal coming back. auto is one a session
// permission made without a prompt, whose failure hands the next ask back to
// a person rather than trying again on every beat.
type renewalAnsweredMsg struct {
	request CredentialRequest
	secret  string
	auto    bool
	key     string
	err     error
}

// renewalSkippedMsg is an automatic renewal that found nothing to do: the
// token's command is no longer the one the session allowed.
type renewalSkippedMsg struct {
	requestID string
}

// sessionKey is what a session permission is kept under: the server, the
// token, and the command exactly as it was allowed.
func sessionKey(server, secretID string, command []string) string {
	return server + "\x00" + secretID + "\x00" + strings.Join(command, "\x00")
}

// refreshSecret finds the token a refresh request is about.
func refreshSecret(req CredentialRequest, secrets []Secret) (Secret, bool) {
	for _, secret := range secrets {
		if secret.ID == req.Refresh.SecretID {
			return secret, true
		}
	}
	return Secret{}, false
}

// askAboutRefresh is the card for a refresh request: which token, why, and the
// ways to give it a new value.
func (m *Model) askAboutRefresh(req CredentialRequest, secrets []Secret) tea.Cmd {
	secret, ok := refreshSecret(req, secrets)
	if !ok {
		m.dialog = errorDialog("Refresh request", "The token this request is about is no longer in the project.")
		return nil
	}
	var items []action
	if len(secret.RefreshCommand) > 0 {
		items = append(items,
			action{key: "once", press: "r", label: "Run once", detail: "run it here and send what it prints", enabled: true},
			action{key: "session", press: "s", label: "Run for this session", detail: "and run it again, unasked, whenever this token needs it while this window is open", enabled: true},
		)
	}
	items = append(items,
		action{key: "enter", press: "e", label: "Enter a value…", detail: "type or paste the new token", enabled: true},
		action{key: "dismiss", press: "d", label: "Dismiss", detail: "keep serving the value on hand; it is asked for again when next needed", enabled: true},
	)
	d := actionsDialog("Refresh request", "", items, func(result string) tea.Cmd {
		switch result {
		case "once":
			return m.renew(req, secret.Name, Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand}, false, "")
		case "session":
			key := sessionKey(m.serverName(req.Server), secret.ID, secret.RefreshCommand)
			if m.renewSession == nil {
				m.renewSession = map[string]bool{}
			}
			m.renewSession[key] = true
			return m.renew(req, secret.Name, Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand, Session: true}, false, key)
		case "enter":
			return m.askForRenewalValue(req, secret, func() tea.Cmd { return m.askAboutRefresh(req, secrets) })
		case "dismiss":
			return m.denyCredential(req)
		}
		return nil
	})
	d.sections = refreshAsk(req, secret, m.secrets.now())
	d.answerLabel = "how should it get a new value?"
	if len(secret.RefreshCommand) > 0 {
		d.footer = "the command runs on this machine, as you, with no shell"
	}
	d.keys = []hint{pressing("enter chooses", "enter"), pressing("esc leaves it waiting", "esc")}
	m.dialog = d
	return nil
}

// refreshAsk is the request as a person reads it before running something:
// the token, why it wants a value, and — above all — the command.
func refreshAsk(req CredentialRequest, secret Secret, now time.Time) []section {
	why := "its value is past the lifetime it was given"
	if req.Refresh.Cause == "rejected" {
		why = "an upstream refused its value"
	}
	fields := []field{
		{label: "token", value: secret.Name, tone: toneAccent},
		{label: "why", value: why},
	}
	if secret.Host != "" {
		fields = append(fields, field{label: "sent to", value: secret.Host})
	}
	if secret.ValueTTL > 0 {
		fields = append(fields, field{label: "a value lasts", value: lifetime.Label(secret.ValueTTL)})
	}
	if req.SandboxID != "" {
		fields = append(fields, field{label: "needed by", value: req.SandboxID, tone: toneDim})
	}
	if when := ago(req.Created, now); when != "" {
		fields = append(fields, field{label: "asked", value: when, tone: toneDim})
	}
	if req.Server != "" {
		fields = append(fields, field{label: "on server", value: req.Server, tone: toneDim})
	}
	sections := []section{{label: "wants a new value", fields: fields}}
	if len(secret.RefreshCommand) > 0 {
		sections = append(sections, section{
			label: "would run",
			lines: []line{{text: refreshcmd.Join(secret.RefreshCommand), tone: toneAccent}},
		})
	}
	return sections
}

// askForRenewalValue collects a value typed in, masked, as a new credential's
// is.
func (m *Model) askForRenewalValue(req CredentialRequest, secret Secret, back func() tea.Cmd) tea.Cmd {
	d := inputDialog("New value", "", "token", "", func(value string) tea.Cmd {
		value = strings.TrimSpace(value)
		if value == "" {
			return m.report(true, "no value entered; the request is still waiting")
		}
		return m.renew(req, secret.Name, Renewal{RequestID: req.ID, SecretID: secret.ID, Value: value}, false, "")
	})
	d.sections = []section{{label: "the token", fields: []field{{label: "token", value: secret.Name, tone: toneAccent}}}}
	d.answerLabel = "paste the new value"
	d.footer = "it replaces the value on hand, and the request is answered with it"
	d.keys = []hint{pressing("Enter accepts", "enter"), pressing("Esc goes back", "esc")}
	d.input.EchoMode = 1 // textinput.EchoPassword: a credential is not drawn back.
	d.onCancel = back
	m.dialog = d
	return nil
}

// renew sends a renewal. A prompted one says what it is doing while the
// command runs; an automatic one runs behind whatever is on screen.
func (m *Model) renew(req CredentialRequest, name string, renewal Renewal, auto bool, key string) tea.Cmd {
	// One renewal per request at a time: a session's own, begun on the beat,
	// may be running while a person answers the same request by hand.
	if m.renewing[req.ID] {
		m.dialog = nil
		return m.report(false, "%s is being renewed already", name)
	}
	if m.renewing == nil {
		m.renewing = map[string]bool{}
	}
	m.renewing[req.ID] = true
	if !auto {
		what := "sending the new value…"
		if len(renewal.Command) > 0 {
			what = "running " + refreshcmd.Join(renewal.Command) + "…"
		}
		m.dialog = statusDialog("Refresh request", what)
	}
	return func() tea.Msg {
		err := m.ds.RefreshSecret(m.ctx, req.Server, renewal)
		return renewalAnsweredMsg{request: req, secret: name, auto: auto, key: key, err: err}
	}
}

// renewalAnswered closes the dialog and re-reads the inbox. A failure is shown
// as the command or the server said it; the request is still waiting.
func (m *Model) renewalAnswered(msg renewalAnsweredMsg) tea.Cmd {
	delete(m.renewing, msg.request.ID)
	// Somebody else's value was written first — another window, another
	// machine, the session's own renewal. The token is renewed; the
	// permission worked; there is nothing to report as a failure.
	if errors.Is(msg.err, ErrAlreadyAnswered) {
		if msg.auto {
			return m.loadCredentialRequests()
		}
		m.dialog = nil
		return tea.Batch(m.loadCredentialRequests(), m.report(false, "%s was already renewed", msg.secret))
	}
	if msg.err != nil {
		// A session permission that fails is not tried again on every beat:
		// the next ask goes to a person, who can see why.
		if msg.key != "" {
			delete(m.renewSession, msg.key)
		}
		if msg.auto {
			return tea.Batch(m.loadCredentialRequests(),
				m.report(true, "could not renew %s: %v", msg.secret, msg.err))
		}
		d := errorDialog("Could not renew", fmt.Sprintf("%v", msg.err))
		d.sections = []section{{label: "still waiting", fields: []field{{label: "token", value: msg.secret, tone: toneAccent}}}}
		m.dialog = d
		return m.loadCredentialRequests()
	}
	if !msg.auto {
		m.dialog = nil
	}
	return tea.Batch(m.loadCredentialRequests(), m.report(false, "renewed %s", msg.secret))
}

// renewBySession answers the refresh requests a session permission covers,
// once each, as the inbox lists them. It reads the token first, because the
// permission is for a command, and the command it would run is the one the
// token names now.
func (m *Model) renewBySession(requests []CredentialRequest) tea.Cmd {
	if len(m.renewSession) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	for _, req := range requests {
		if req.Refresh == nil || m.renewing[req.ID] {
			continue
		}
		server := m.serverName(req.Server)
		// The permissions for this token, as they stand now, go with the
		// command: it runs off the update loop, which is the only place they
		// change.
		prefix := server + "\x00" + req.Refresh.SecretID + "\x00"
		allowed := map[string]bool{}
		for key := range m.renewSession {
			if strings.HasPrefix(key, prefix) {
				allowed[key] = true
			}
		}
		if len(allowed) == 0 {
			continue
		}
		if m.renewing == nil {
			m.renewing = map[string]bool{}
		}
		m.renewing[req.ID] = true
		cmds = append(cmds, func() tea.Msg {
			secrets, err := m.ds.Secrets(m.ctx, req.Server)
			if err != nil {
				return renewalSkippedMsg{requestID: req.ID}
			}
			secret, ok := refreshSecret(req, secrets)
			if !ok || len(secret.RefreshCommand) == 0 {
				return renewalSkippedMsg{requestID: req.ID}
			}
			key := sessionKey(server, secret.ID, secret.RefreshCommand)
			if !allowed[key] {
				return renewalSkippedMsg{requestID: req.ID}
			}
			err = m.ds.RefreshSecret(m.ctx, req.Server, Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand, Session: true})
			return renewalAnsweredMsg{request: req, secret: secret.Name, auto: true, key: key, err: err}
		})
	}
	return tea.Batch(cmds...)
}
