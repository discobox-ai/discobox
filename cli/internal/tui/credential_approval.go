package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Each part has its own decision and scope. Nothing is sent until both have
// been reviewed; canceling leaves the whole request pending on the server.
// What the card asks and accepts is the core's (console.ApprovalPartForm).
func (m *Model) approvalPart(a approval, delegate bool) tea.Cmd {
	holder, pinned := console.HeldBy(a.held)
	pinned = pinned && !delegate
	locked := delegate && a.use == console.PartApprove
	scope := a.useScope
	if delegate {
		scope = a.delegateScope
	}
	ask := console.ApprovalPartAsk{
		Request: a.req, Secrets: a.secrets, Held: a.held, Delegate: delegate, Settled: locked, PartsChosen: a.partsChosen,
		Secret: a.secret, Fresh: a.fresh, TTL: a.ttl, Scope: scope, As: a.useAs, Raw: a.raw,
	}
	q := console.ApprovalPartForm(ask)
	f := newForm(q.Fields...)
	title := "Allow credential use?"
	if delegate {
		title = "Allow credential delegation?"
	}
	part := a.req.Use
	if delegate {
		part = a.req.Delegate
	}
	if part == nil {
		part = &console.RequestPart{Routes: a.req.Routes()}
	}
	hosts := secretroute.OpenRouteHosts(part.Routes)
	// apply is the approval as the card's answers make it.
	apply := func(p console.ApprovalPart) approval {
		n := a
		n.fresh, n.secret, n.ttl = p.Fresh, p.Secret, p.TTL
		if delegate {
			n.delegateScope = p.Scope
		} else {
			n.useScope, n.useAs, n.raw = p.Scope, p.As, p.Raw
		}
		return n
	}
	read := func() approval { return apply(console.ReadApprovalPart(ask, f.answers())) }
	var d *dialog
	d = approvalDialog(title, f, func(f *form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "raw" {
			return m.showRawRequest(a.req, func() tea.Cmd { m.dialog = d; return nil })
		}
		decision := console.PartApprove
		n := read()
		if d.formActions[d.formActionAt].key == "deny" {
			decision = console.PartDeny
		} else {
			p, ok := answer(f, q)
			if !ok {
				return nil
			}
			n = apply(p)
		}
		if delegate {
			n.delegate = decision
		} else {
			n.use = decision
			if decision == console.PartDeny {
				n.raw = ""
			}
		}
		if !delegate && a.req.Delegate != nil {
			n.partsChosen = true
			return m.approvalPart(n, true)
		}
		n.partsChosen = true
		if n.use != console.PartApprove && n.delegate != console.PartApprove {
			return m.denyCredential(a.req)
		}
		if n.fresh {
			return m.askNewCredentialForm(n)
		}
		return m.chooseSecret(n, n.secret.ID)
	})
	if a.req.Raw != "" {
		d.formActions = append(d.formActions, action{key: "raw", label: "Request JSON", enabled: true})
	}
	d.titleRight = "1 of 1"
	if a.req.AsksBoth() {
		d.titleRight = "1 of 2"
		if delegate {
			d.titleRight = "2 of 2"
		}
	}
	d.refreshSections = func() {
		answered := console.ReadApprovalPart(ask, f.answers())
		next := apply(answered)
		d.sections = []section{{fields: []field{{label: "Box", value: a.req.SandboxID, tone: toneAccent}, {label: "Reason (box's words)", value: a.req.Justification}}}}
		if a.req.Server != "" {
			d.sections[0].fields = append(d.sections[0].fields, field{label: "Server", value: a.req.Server})
		}
		if !delegate {
			d.sections[0].fields = append(d.sections[0].fields, field{label: "Variable", value: console.VariableOf(a.req)})
		}
		explain := "This box may use the credential."
		if delegate {
			explain = "This box may give access to boxes it leads; delegation grants no use to this box."
		}
		d.sections = append(d.sections, section{lines: []line{{text: explain}}})
		if pinned {
			d.sections = append(d.sections, section{lines: []line{{text: console.VariableOf(a.req) + " already holds " + console.HeldSecret(a.secrets, holder).Name + " in this box; no other credential can be granted there, so it answers this.", tone: toneAccent}}})
		}
		if delegate && a.req.Use != nil {
			state := "Use denied. Credential and duration remain editable."
			if locked {
				state = "Use accepted for this answer. Credential and duration are locked. Nothing has been sent yet."
			}
			d.sections = append(d.sections, section{lines: []line{{text: state, tone: toneAccent}}})
		}
		d.sections = append(d.sections, approvalGrantSection(answered.Scope, part, hosts))
		for _, role := range part.Roles {
			d.sections = append(d.sections, section{label: "Requested role (box's words)", lines: roleLines(role)})
		}
		limits := section{label: "Credential and parent ceilings", lines: []line{{text: "Every independent restriction still applies. Approval cannot widen a parent delegation."}}}
		if next.fresh {
			limits.lines = append(limits.lines, line{text: "Credential restrictions and lifetime limit are chosen on the new credential form."})
		} else {
			limits.fields = append(limits.fields, field{label: next.secret.Name, value: console.RouteRows(next.secret.Routes)})
			for i, list := range next.secret.RouteConstraints {
				limits.fields = append(limits.fields, field{label: fmt.Sprintf("Constraint %d", i+1), value: console.RouteRows(list)})
			}
			if next.secret.MaxTTL > 0 {
				limits.fields = append(limits.fields, field{label: "Maximum duration", value: lifetime.Label(next.secret.MaxTTL)})
			}
		}
		d.sections = append(d.sections, limits)
		if !delegate {
			effect := "Replace existing use grants in " + console.VariableOf(a.req) + " for " + lifetime.Label(next.ttl) + ". The final review shows each affected grant."
			if next.useAs == bindingplan.Add {
				effect = "Add these routes for " + lifetime.Label(next.ttl) + ". Existing grants keep their routes and lifetimes."
			}
			d.sections = append(d.sections, section{label: "What accepting changes", fields: []field{{label: "Variable", value: console.VariableOf(a.req)}}, lines: []line{{text: effect}, {text: rawDeliveryText(next.raw), tone: toneAlert}}})
			if next.raw == "" {
				d.sections[len(d.sections)-1].lines[1] = line{text: "Placeholder delivery: the box never receives the credential value."}
			}
		}
	}
	d.refreshSections()
	m.dialog = d
	return nil
}

func (m *Model) confirmCredentialApproval(a approval) tea.Cmd {
	var d *dialog
	d = approvalDialog("Confirm credential access", newForm(), func(*form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "deny" {
			return m.denyCredential(a.req)
		}
		return m.finishApproval(a)
	})
	d.sections = []section{bindingSection(a)}
	for _, parent := range a.parents {
		value := console.RouteRows(parent.Routes)
		if len(parent.Routes) == 0 {
			value = "No additional parent route restriction"
		}
		d.sections = append(d.sections, section{label: "Parent delegation ceiling", fields: []field{{label: parent.ID, value: value}, {label: "Expires", value: parent.Expires.String()}}})
	}
	if a.use == console.PartApprove {
		d.sections = append(d.sections, heldSection(a))
	}
	d.sections = append(d.sections, section{lines: []line{{text: "This submits the use and delegation decisions together. Deny refuses the entire request; Esc leaves it waiting."}}})

	m.dialog = d
	return nil
}
