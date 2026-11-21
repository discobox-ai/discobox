package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

type approvalBindingsMsg struct {
	wait     *dialog
	bindings []SecretBinding
	err      error
	done     func([]SecretBinding, error) tea.Cmd
}

func (m *Model) askCredentialApproval(req CredentialRequest, secrets []Secret, held []HeldGrant) tea.Cmd {
	ttl := defaultRequestTTL(req)
	done := func(bindings []SecretBinding, err error) tea.Cmd {
		if err != nil {
			m.dialog = errorDialog("Credential request", fmt.Sprintf("Cannot read the box's bindings: %v", err))
			return nil
		}
		return m.credentialApproval(req, secrets, held, bindings, ttl)
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

func (m *Model) credentialApproval(req CredentialRequest, secrets []Secret, held []HeldGrant, bindings []SecretBinding, ttl time.Duration) tea.Cmd {
	holder, pinned := heldBy(held)
	answers := map[string]requestAnswer{}
	var credentials []choice
	ordered := secretsForRequest(secrets, req)
	if holder.SecretID != "" && !slices.ContainsFunc(ordered, func(s Secret) bool { return s.ID == holder.SecretID }) {
		ordered = append([]Secret{heldSecret(secrets, holder)}, ordered...)
	}
	for _, secret := range ordered {
		readable := slices.ContainsFunc(secrets, func(s Secret) bool { return s.ID == secret.ID })
		if cannotAnswer(secret, req, holder, pinned, readable) != "" {
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
	if len(credentials) == 0 {
		credentials = append(credentials, choice{label: "No compatible saved credential"})
	}
	durations := lifetimeChoices(req.AskedTTL)
	if !slices.Contains(durations, ttl) {
		durations = append(slices.Clone(durations), ttl)
	}
	var lifetimes []choice
	for _, duration := range durations {
		key := lifetime.Label(duration)
		label := key
		if duration > 0 && duration == req.AskedTTL {
			label += " · requested"
		}
		lifetimes = append(lifetimes, choice{key: key, label: label})
	}
	lifetimes = append(lifetimes, choice{key: "custom", label: "Custom…"})
	customTTL := textRow("custom-ttl", "Custom lifetime", "90m, 3d or forever", "")
	customTTL.when = func(f *form) bool { return f.chosen("ttl") == "custom" }
	selectedLifetime := func(f *form) (time.Duration, error) {
		value := f.chosen("ttl")
		if value == "custom" {
			value = f.value("custom-ttl")
		}
		return lifetime.Parse(value)
	}
	var d *dialog
	rows := []formRow{pickRow("credential", "Credential", credentials...), pickRow("ttl", "Lifetime", lifetimes...), customTTL}
	if req.Use != nil {
		rows = append(rows, pickRow("binding", "Existing bindings",
			choice{key: string(bindingplan.Replace), label: "Replace existing bindings"},
			choice{key: string(bindingplan.Add), label: "Append to existing bindings"}))
		rows[len(rows)-1].when = func(f *form) bool {
			ans, ok := answers[f.chosen("credential")]
			return ok && f.chosen("use-decision") == PartApprove && ans.canAdd() && ans.replaceUnavailable() == ""
		}
	}
	if req.Use == nil || !pinned {
		rows = append(rows, formRow{key: "new", label: "New credential", show: func(*form) string { return "Enter a value…" }, open: func() tea.Cmd {
			back := func() tea.Cmd { m.dialog = d; return nil }
			return m.enterNewCredential(req, secrets, held, holder.SecretID, back, func(ans requestAnswer) tea.Cmd {
				answers["new"] = ans
				options := slices.DeleteFunc(slices.Clone(credentials), func(c choice) bool { return c.key == "" })
				d.form.rows[0].choices = append(options, choice{key: "new", label: ans.credential + " (new)"})
				d.form.choose("credential", "new")
				d.reviewState = ""
				return back()
			})
		}})
	}
	parts := []struct {
		key, label string
		part       *RequestPart
	}{{"use", "Access for this box", req.Use}, {"delegate", "Access for its workers", req.Delegate}}
	rows = append(rows, pickRow("advanced", "Advanced",
		choice{key: "hide", label: "Show part decisions and route scope…"},
		choice{key: "show", label: "Hide part decisions and route scope"}))
	editedRoutes := map[string][]secretroute.Route{}
	for _, part := range parts {
		if part.part == nil {
			continue
		}
		decision := pickRow(part.key+"-decision", part.label,
			choice{key: PartApprove, label: "Approve"}, choice{key: PartDeny, label: "Deny"})
		decision.when = func(f *form) bool { return f.chosen("advanced") == "show" }
		scopes := []choice{{key: "routes", label: "Requested routes"}}
		if len(secretroute.OpenRouteHosts(part.part.Routes)) > 0 {
			scopes = append(scopes, choice{key: "hosts", label: "Requested hosts · any method and path"})
		}
		scopes = append(scopes, choice{key: "anywhere", label: "Anywhere · limited by credential scope"},
			choice{key: "edited", label: "Edited routes"})
		scope := pickRow(part.key+"-scope", "Route scope", scopes...)
		scope.when = func(f *form) bool {
			return f.chosen("advanced") == "show" && f.chosen(part.key+"-decision") == PartApprove
		}
		edit := formRow{key: part.key + "-edit", label: "Edited routes", show: func(*form) string { return "Open route editor…" }}
		edit.when = func(f *form) bool { return scope.when(f) && f.chosen(part.key+"-scope") == "edited" }
		edit.open = func() tea.Cmd {
			routes := editedRoutes[part.key]
			if len(routes) == 0 {
				routes = part.part.Routes
			}
			back := func() tea.Cmd { m.dialog = d; return nil }
			return m.editRoutes(req, part.label, routesForEditor(routes, part.label, d.form.chosenLabel("credential")), back, func(routes []secretroute.Route) tea.Cmd {
				editedRoutes[part.key] = routes
				d.reviewState = ""
				return back()
			})
		}
		rows = append(rows, decision, scope, edit)
	}
	f := newForm(rows...)
	f.choose("ttl", lifetime.Label(ttl))
	if req.Use != nil && req.Use.Add {
		f.choose("binding", string(bindingplan.Add))
	}
	answer := func(f *form) (requestAnswer, string) {
		ans, ok := answers[f.chosen("credential")]
		ans.use, ans.delegate = partAnswer(req.Use, f.chosen("use-decision")), partAnswer(req.Delegate, f.chosen("delegate-decision"))
		ans.useChoice = partChoice{scope: f.chosen("use-scope"), routes: editedRoutes["use"], as: bindingplan.As(f.chosen("binding"))}
		ans.delegateChoice = partChoice{scope: f.chosen("delegate-scope"), routes: editedRoutes["delegate"]}
		var err error
		ans.ttl, err = selectedLifetime(f)
		switch {
		case !ok:
			return ans, "Choose a credential to accept."
		case err != nil:
			return ans, err.Error()
		case !withinLimit(ans.secret, ans.ttl):
			return ans, "This credential allows at most " + lifetime.Label(ans.maxTTL) + "."
		case (ans.use == PartApprove && ans.useChoice.scope == "edited" && len(ans.useChoice.routes) == 0) ||
			(ans.delegate == PartApprove && ans.delegateChoice.scope == "edited" && len(ans.delegateChoice.routes) == 0):
			return ans, "Enter the edited routes before approving."
		case ans.use == PartApprove && ans.useAs(req) == bindingplan.Replace && ans.replaceUnavailable() != "":
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
		for _, item := range parts {
			if item.part == nil {
				continue
			}
			decision, selected := ans.use, ans.useChoice
			if item.key == "delegate" {
				decision, selected = ans.delegate, ans.delegateChoice
			}
			if decision == PartDeny {
				d.sections = append(d.sections, section{label: item.label, lines: []line{{text: "Deny this part"}}})
			} else {
				d.sections = append(d.sections, choiceSection(item.label, item.part, selected, ans))
			}
			if len(item.part.Roles) > 0 {
				roles := section{label: "the roles it asked for · the discobox's words"}
				for _, role := range item.part.Roles {
					roles.lines = append(roles.lines, roleLines(role)...)
				}
				if decision == PartDeny || selected.scope != "routes" {
					roles.lines = append(roles.lines, line{text: "These roles describe the request.", tone: toneDim})
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
		if ans.use == PartApprove {
			effect := "Approving replaces any existing rules for “" + ans.credential + "” in this box, across all variables."
			if ans.useAs(req) == bindingplan.Add {
				effect = "Approving adds these routes alongside the existing rules for “" + ans.credential + "” in this box."
			}
			d.sections = append(d.sections, section{lines: []line{{text: effect}}})
		}
		if ans.delegate == PartApprove {
			d.sections = append(d.sections, section{lines: []line{{text: "Approval " + ans.delegateMode() + "."}}})
		}
		d.sections = append(d.sections, approvalConsequences(req, ans)...)
	}
	d.refreshSections()
	m.withRawRequest(d, req)
	m.dialog = d
	return nil
}
