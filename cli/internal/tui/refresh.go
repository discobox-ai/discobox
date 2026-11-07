package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
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
	request console.CredentialRequest
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
func refreshSecret(req console.CredentialRequest, secrets []console.Secret) (console.Secret, bool) {
	for _, secret := range secrets {
		if secret.ID == req.Refresh.SecretID {
			return secret, true
		}
	}
	return console.Secret{}, false
}

// askAboutRefresh is the card for a refresh request: which token, why, and the
// ways to give it a new value.
func (m *Model) askAboutRefresh(req console.CredentialRequest, secrets []console.Secret) tea.Cmd {
	secret, ok := refreshSecret(req, secrets)
	if !ok {
		m.dialog = errorDialog("Refresh request", "The token this request is about is no longer in the project.")
		return nil
	}
	source := pickRow("source", "New value", choice{key: "command", label: "Run saved command", hint: "Run locally as you, without a shell"}, choice{key: "manual", label: "Paste a value", hint: "Enter a replacement token; the field stays masked"})
	if len(secret.RefreshCommand) == 0 {
		source.choices = source.choices[1:]
	}
	permission := pickRow("permission", "Run permission", choice{key: "once", label: "Once"}, choice{key: "session", label: "While this window is open", hint: "Run again without asking for this exact credential and command"})
	permission.when = func(f *form) bool { return f.chosen("source") == "command" }
	permission.why = "No command runs for a pasted value"
	value := textRow("value", "Token", "Paste the replacement token", "")
	value.masked = true
	value.when = func(f *form) bool { return f.chosen("source") == "manual" }
	value.why = "The command supplies the value"
	f := newForm(source, permission, value)
	var d *dialog
	d = approvalDialog("Refresh "+secret.Name+"?", f, func(*form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "dismiss" {
			f.set("value", "")
			return m.denyCredential(req)
		}
		if f.chosen("source") == "manual" {
			token := strings.TrimSpace(f.value("value"))
			if token == "" {
				f.err = "Enter a replacement token."
				return nil
			}
			f.set("value", "")
			return m.renew(req, secret.Name, console.Renewal{RequestID: req.ID, SecretID: secret.ID, Value: token}, false, "")
		}
		renewal := console.Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand}
		key := ""
		if f.chosen("permission") == "session" {
			key = sessionKey(m.session.ServerName(req.Server), secret.ID, secret.RefreshCommand)
			if m.renewSession == nil {
				m.renewSession = map[string]bool{}
			}
			m.renewSession[key] = true
			renewal.Session = true
		}
		return m.renew(req, secret.Name, renewal, false, key)
	})
	d.formActions = []action{{key: "refresh", label: "Refresh", enabled: true}, {key: "dismiss", label: "Dismiss", enabled: true}}
	d.onCancel = func() tea.Cmd { f.set("value", ""); return nil }
	d.refreshSections = func() {
		d.sections = refreshAsk(req, secret, m.secrets.now())
		if f.chosen("source") == "manual" {
			d.sections = append(d.sections, section{label: "Paste a replacement", lines: []line{{text: "No command will run. The masked value replaces the project token."}}})
		} else {
			permission := "Run once, then ask again next time."
			if f.chosen("permission") == "session" {
				permission = "Run this exact command again without asking while this console window stays open."
			}
			d.sections = append(d.sections, section{label: "Command permission", lines: []line{{text: "Runs locally as you, without a shell."}, {text: permission, tone: toneAccent}}})
		}
		d.sections = append(d.sections, section{label: "What refreshing changes", lines: []line{{text: "Replaces the project credential's value. Existing grants keep their routes and lifetimes."}, {text: "Dismiss keeps the current value; the request may return when the credential is needed again."}}})
	}
	d.refreshSections()
	m.dialog = d
	return nil
}

// refreshAsk is the request as a person reads it before running something:
// the token, why it wants a value, and — above all — the command.
func refreshAsk(req console.CredentialRequest, secret console.Secret, now time.Time) []section {
	why := "its value is past the lifetime it was given"
	if req.Refresh.Cause == "rejected" {
		why = "an upstream refused its value"
	}
	fields := []field{
		{label: "token", value: secret.Name, tone: toneAccent},
		{label: "why", value: why},
	}
	if len(secret.Routes) > 0 {
		fields = append(fields, field{label: "sent to", value: console.RouteRows(secret.Routes)})
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

// renew sends a renewal. A prompted one says what it is doing while the
// command runs; an automatic one runs behind whatever is on screen.
func (m *Model) renew(req console.CredentialRequest, name string, renewal console.Renewal, auto bool, key string) tea.Cmd {
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
	if errors.Is(msg.err, console.ErrAlreadyAnswered) {
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
func (m *Model) renewBySession(requests []console.CredentialRequest) tea.Cmd {
	if len(m.renewSession) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	for _, req := range requests {
		if req.Refresh == nil || m.renewing[req.ID] {
			continue
		}
		server := m.session.ServerName(req.Server)
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
			err = m.ds.RefreshSecret(m.ctx, req.Server, console.Renewal{RequestID: req.ID, SecretID: secret.ID, Command: secret.RefreshCommand, Session: true})
			return renewalAnsweredMsg{request: req, secret: secret.Name, auto: true, key: key, err: err}
		})
	}
	return tea.Batch(cmds...)
}
