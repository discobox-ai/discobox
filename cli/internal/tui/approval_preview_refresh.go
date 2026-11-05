//go:build prototype

package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Renewal changes a project token's value, not the box's authorization.
// This fixture never runs its command, sends or retains an entered value.
func (p *approvalPreview) openRefresh() {
	now := time.Now()
	secret := Secret{ID: "sec_cloud", Name: "Cloud access token", Type: "bearer", ValueTTL: time.Hour, RefreshCommand: []string{"gcloud", "auth", "print-access-token"}, Routes: []secretroute.Route{{Hosts: []string{"container.googleapis.com"}}}}
	if p.noCommand {
		secret.RefreshCommand = nil
	}
	cause, reason := "stale", "The token has expired."
	if p.readOnly {
		cause = "rejected"
		reason = "The upstream service rejected the token."
	}
	req := CredentialRequest{ID: "refresh_sample", SandboxID: "sbx_worker", Name: secret.Name, Created: now.Add(-2 * time.Minute), Refresh: &RefreshAsk{SecretID: secret.ID, Cause: cause}}
	if p.current {
		m := &Model{secrets: newSecretList()}
		m.askAboutRefresh(req, []Secret{secret})
		p.d = m.dialog
		p.d.action = func(result string) tea.Cmd { return p.receipt("Refresh · " + result) }
		return
	}
	source := pickRow("source", "New value", choice{key: "command", label: "Run saved command", hint: "Runs on your machine, as you, without a shell. Sends its output as the new token."}, choice{key: "manual", label: "Paste a value", hint: "Enter a replacement token. It stays masked and is never echoed in the preview receipt."})
	if p.noCommand {
		source.choices = source.choices[1:]
	}
	permission := pickRow("permission", "Run permission", choice{key: "once", label: "Once", hint: "Ask again the next time this token needs refreshing."}, choice{key: "session", label: "While this window is open", hint: "Run this command again without asking when this token needs refreshing, until this console window closes."})
	permission.when = func(f *form) bool { return f.chosen("source") == "command" }
	permission.why = "No command runs for a pasted value"
	value := textRow("value", "Token", "Sample value only — do not paste a real token", "")
	value.masked = true
	value.when = func(f *form) bool { return f.chosen("source") == "manual" }
	value.why = "The command supplies the new value"
	f := newForm(source, permission, value)
	var d *dialog
	d = formDialog("Refresh Cloud access token?", f, func(f *form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "dismiss" {
			return p.receipt("Refresh · dismissed · current value unchanged")
		}
		if f.chosen("source") == "manual" {
			if strings.TrimSpace(f.value("value")) == "" {
				f.err = "Enter a sample token first."
				return nil
			}
			f.set("value", "")
			return p.receipt("Refresh · pasted value accepted · value hidden")
		}
		return p.receipt("Refresh · command approved · " + f.chosenLabel("permission") + " · no command executed")
	})
	d.titleRight = "2m ago"
	d.sections = []section{
		{fields: []field{{label: "Box", value: "Deploy worker · sbx_worker", tone: toneAccent}, {label: "Credential", value: secret.Name, tone: toneAccent}, {label: "Reason", value: reason}, {label: "Sent to", value: "container.googleapis.com"}, {label: "Value lifetime", value: "1 hour"}}},
		{label: "Renewal action"},
		{label: "What refreshing changes", lines: []line{{text: "Replaces the project credential's value; existing grants keep their routes and lifetimes."}, {text: "Dismissing keeps the current value. The request may return when the token is needed again."}}},
	}
	p.refreshGrant = func() {
		if p.d != d {
			return
		}
		if f.chosen("source") == "manual" {
			d.sections[1] = section{label: "Paste a replacement", lines: []line{{text: "Enter the new token in the masked field below. No command will run.", tone: toneAccent}}}
			return
		}
		permission := "Run once, then ask again for the next refresh."
		if f.chosen("permission") == "session" {
			permission = "Run again without asking while this console window remains open."
		}
		d.sections[1] = section{label: "Command to run on your machine", lines: []line{{text: refreshcmd.Join(secret.RefreshCommand), tone: toneAccent}, {text: "Runs as you, without a shell. Its output replaces the token."}, {text: permission, tone: toneAccent}}}
	}
	d.formActions = []action{{key: "refresh", label: "Refresh", enabled: true}, {key: "dismiss", label: "Dismiss", enabled: true}}
	d.formActionAt = -1
	d.keys = []hint{says("Tab / ↑↓ move"), says("←→ change"), pressing("enter selects", "enter"), pressing("esc leaves waiting", "esc")}
	d.footer = "Sample only · use dummy text · no command runs and no token is sent"
	p.d = d
	p.refreshGrant()
}
