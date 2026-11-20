package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
)

type approvalBindingsMsg struct {
	wait     *dialog
	bindings []SecretBinding
	err      error
	done     func([]SecretBinding, error) tea.Cmd
}

func (m *Model) askWhichCredential(req CredentialRequest, secrets []Secret, held []HeldGrant) tea.Cmd {
	ttl := defaultRequestTTL(req)
	done := func(bindings []SecretBinding, err error) tea.Cmd {
		if err != nil {
			m.dialog = errorDialog("Credential request", fmt.Sprintf("Cannot read the box's bindings: %v", err))
			return nil
		}
		return m.credentialApproval(req, secrets, held, bindings, requestAnswer{}, ttl)
	}
	if req.SandboxID == "" {
		return done(nil, nil)
	}
	wait := statusDialog("Credential request", "Reading existing bindings…")
	m.dialog = wait
	ctx, ds := m.ctx, m.ds
	return func() tea.Msg {
		bindings, err := ds.Bindings(ctx, req.Server, BindingFilter{})
		return approvalBindingsMsg{wait: wait, bindings: bindings, err: err, done: done}
	}
}

func (m *Model) credentialApproval(req CredentialRequest, secrets []Secret, held []HeldGrant, bindings []SecretBinding, fresh requestAnswer, ttl time.Duration) tea.Cmd {
	holder, pinned := heldBy(held)
	answers := map[string]requestAnswer{}
	var credentials []choice
	for _, secret := range secretsForRequest(secrets, req) {
		if cannotAnswer(secret, req, holder, pinned, true) != "" {
			continue
		}
		ans := requestAnswer{credential: secret.Name, secret: secret, credentialRoutes: secret.Routes, maxTTL: secret.MaxTTL, held: held, holderID: holder.SecretID}
		for _, b := range bindings {
			if b.SecretID == secret.ID && b.TargetKind == BindingSandbox && b.TargetID == req.SandboxID {
				ans.standing = append(ans.standing, b)
			}
		}
		answers[secret.ID] = ans
		credentials = append(credentials, choice{key: secret.ID, label: secret.Name})
	}
	if fresh.fresh {
		answers["new"] = fresh
		credentials = append([]choice{{key: "new", label: fresh.credential + " (new)"}}, credentials...)
	}
	if len(credentials) == 0 {
		credentials = append(credentials, choice{label: "No compatible saved credential"})
	}
	ttlText := ttl.String()
	if ttl <= 0 {
		ttlText = "forever"
	} else if ttl%time.Hour == 0 {
		ttlText = fmt.Sprintf("%dh", ttl/time.Hour)
	}
	var d *dialog
	rows := []formRow{pickRow("credential", "Credential", credentials...), textRow("ttl", "Lifetime", "1h, 3d or forever", ttlText)}
	if req.Use != nil {
		rows = append(rows, pickRow("binding", "Existing bindings",
			choice{key: string(bindingplan.Replace), label: "Replace existing bindings"},
			choice{key: string(bindingplan.Add), label: "Append to existing bindings"}))
		rows[len(rows)-1].when = func(f *form) bool {
			ans, ok := answers[f.chosen("credential")]
			return ok && ans.canAdd() && ans.replaceUnavailable() == ""
		}
	}
	if req.Use == nil || !pinned {
		rows = append(rows, formRow{key: "new", label: "New credential", show: func(*form) string { return "Enter a value…" }, open: func() tea.Cmd {
			selectedTTL, err := lifetime.Parse(d.form.value("ttl"))
			if err != nil {
				selectedTTL = ttl
			}
			return m.enterNewCredential(req, secrets, held, holder.SecretID, selectedTTL, func() tea.Cmd { m.dialog = d; return nil }, func(ans requestAnswer) tea.Cmd {
				return m.credentialApproval(req, secrets, held, bindings, ans, selectedTTL)
			})
		}})
	}
	f := newForm(rows...)
	if req.Use != nil && req.Use.Add {
		f.choose("binding", string(bindingplan.Add))
	}
	answer := func(f *form) (requestAnswer, string) {
		ans, ok := answers[f.chosen("credential")]
		ans.use, ans.delegate = partAnswer(req.Use, PartApprove), partAnswer(req.Delegate, PartApprove)
		ans.useChoice = partChoice{scope: "routes", as: bindingplan.As(f.chosen("binding"))}
		ans.delegateChoice.scope = "routes"
		var err error
		ans.ttl, err = lifetime.Parse(f.value("ttl"))
		switch {
		case !ok:
			return ans, "Choose a credential to accept."
		case err != nil:
			return ans, err.Error()
		case !withinLimit(ans.secret, ans.ttl):
			return ans, "This credential allows at most " + lifetime.Label(ans.maxTTL) + "."
		case req.Use != nil && ans.useAs(req) == bindingplan.Replace && ans.replaceUnavailable() != "":
			return ans, ans.replaceUnavailable()
		}
		return ans, ""
	}
	d = approvalDialog("Credential request", f, func(f *form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "deny" {
			return m.denyCredential(req)
		}
		ans, why := answer(f)
		if why != "" {
			f.err = why
			return nil
		}
		return m.partsAnswered(req, ans)
	})
	d.titleRight = "approval"
	d.body = m.credentialSentence(req)
	d.refreshSections = func() {
		ans, why := answer(f)
		d.formActions[0].enabled = why == ""
		d.sections = nil
		if req.Server != "" {
			d.sections = append(d.sections, section{fields: []field{{label: "Server", value: req.Server, tone: toneAccent}}})
		}
		for _, item := range []struct {
			label string
			part  *RequestPart
		}{{"Its own use", req.Use}, {"Its workers", req.Delegate}} {
			if item.part == nil {
				continue
			}
			if route := singleHTTPRoute(item.part.Routes); route != "" {
				d.sections = append(d.sections, section{fields: []field{{label: item.label, value: route, tone: toneAccent}}})
			} else {
				d.sections = append(d.sections, approvalRouteSection(item.label, item.part))
			}
			if len(item.part.Roles) > 0 {
				roles := section{label: "the roles it asked for · the discobox's words"}
				for _, role := range item.part.Roles {
					roles.lines = append(roles.lines, roleLines(role)...)
				}
				d.sections = append(d.sections, roles)
			}
		}
		if reason := strings.TrimSpace(req.Justification); reason != "" {
			d.sections = append(d.sections, section{fields: []field{{label: "Purpose", value: reason}}})
		}
		if why != "" {
			d.sections = append(d.sections, section{lines: []line{{text: why, tone: toneAlert}}})
			return
		}
		if req.Use != nil {
			d.sections = append(d.sections, section{lines: []line{{text: "Its own use " + ans.useMode(req) + "."}}})
		}
		if req.Delegate != nil {
			d.sections = append(d.sections, section{lines: []line{{text: "Approval " + ans.delegateMode() + "."}}})
		}
		d.sections = append(d.sections, approvalConsequences(req, ans, false)...)
	}
	d.refreshSections()
	m.withRawRequest(d, req)
	m.dialog = d
	return nil
}
