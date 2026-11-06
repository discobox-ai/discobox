package tui

import (
	"fmt"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Each part has its own decision and scope. Nothing is sent until both have
// been reviewed; canceling leaves the whole request pending on the server.
//
// A variable the discobox's own bindings already deliver a secret in is
// answered, on the use card, with that secret alone: the server refuses any
// other there (heldBy), so the Credential row is locked to it, with no new
// credential offered.
func (m *Model) approvalPart(a approval, delegate bool) tea.Cmd {
	holder, pinned := heldBy(a.held)
	pinned = pinned && !delegate
	var credentials []choice
	if pinned {
		held := heldSecret(a.secrets, holder)
		credentials = []choice{{key: held.ID, label: held.Name, hint: "Already in " + variableOf(a.req) + " in this box"}}
	} else {
		for _, s := range secretsForRequest(a.secrets, a.req) {
			credentials = append(credentials, choice{key: s.ID, label: s.Name, hint: secretDetail(s)})
		}
		credentials = append(credentials, choice{key: "new", label: "New credential…", hint: "Enter its value after reviewing both parts"})
	}
	credential := pickRow("credential", "Credential", credentials...)
	var durations []choice
	offered := a.req.AskedTTL
	if a.partsChosen {
		offered = a.ttl
	}
	for _, ttl := range lifetimeChoices(offered) {
		durations = append(durations, choice{key: strconv.FormatInt(lifetime.Seconds(ttl), 10), label: lifetime.Label(ttl)})
	}
	durations = append(durations, choice{key: "custom", label: "Custom…", hint: "Type a duration such as 90m or 3d"})
	duration := pickRow("duration", "Duration", durations...)
	custom := textRow("customDuration", "Custom duration", "90m, 3d, 6mo or forever", "")
	custom.when = func(f *form) bool { return f.chosen("duration") == "custom" }
	custom.why = "Choose Custom to type a duration"
	part, title := a.req.Use, "Allow credential use?"
	if delegate {
		part, title = a.req.Delegate, "Allow credential delegation?"
	}
	if part == nil {
		part = &RequestPart{Routes: a.req.routes()}
	}
	hosts := secretroute.OpenRouteHosts(part.Routes)
	scope := pickRow("scope", "Scope", choice{key: "routes", label: "Requested routes", hint: "Keep methods, paths, protocol restrictions and blocked routes"}, choice{key: "hosts", label: "Requested hosts", hint: "Any method, path and protocol on open requested hosts; drops request blocks"}, choice{key: "anywhere", label: "Anywhere", hint: "No grant restriction; credential and parent limits still apply"})
	rows := []formRow{credential, duration, custom, scope}
	// Use choices belong to this part alone; delegation never changes delivery.
	if !delegate {
		rows = append(rows, pickRow("as", "Grant as", choice{key: string(bindingplan.Replace), label: "Replacement", hint: "Replaces existing use grants in this variable"}, choice{key: string(bindingplan.Add), label: "Addition", hint: "Existing grants remain; these routes have their own lifetime"}))
		rows = append(rows, pickRow("delivery", "Delivery", choice{key: "sentinel", label: "Placeholder", hint: "The pool swaps in the value on its permitted routes"}, choice{key: "env", label: "Value in variable", hint: "Only if the credential allows raw delivery"}, choice{key: "file", label: "Value in memory file", hint: "Only if the credential allows raw delivery"}))
	}
	f := newForm(rows...)
	locked := delegate && a.use == PartApprove
	f.rows[0].locked, f.rows[1].locked = locked || pinned, locked
	if a.secret.ID != "" {
		f.choose("credential", a.secret.ID)
	} else if a.fresh {
		f.choose("credential", "new")
	}
	ttl := a.ttl
	if !a.partsChosen {
		ttl = a.req.AskedTTL
		if ttl <= 0 || ttl%time.Second != 0 {
			ttl = agentcreds.DefaultGrantTTL
		}
	}
	f.choose("duration", strconv.FormatInt(lifetime.Seconds(ttl), 10))
	f.moveTo(0)
	if !delegate {
		f.choose("as", string(a.useAs))
		if a.raw != "" {
			f.choose("delivery", a.raw)
		}
	}
	chosenScope := a.useScope
	if delegate {
		chosenScope = a.delegateScope
	}
	if chosenScope != "" {
		f.choose("scope", chosenScope)
	}
	var d *dialog
	read := func() approval {
		next := a
		next.fresh = f.chosen("credential") == "new"
		if !locked {
			next.secret = Secret{}
			if pinned {
				next.secret = heldSecret(a.secrets, holder)
			}
			for _, s := range a.secrets {
				if s.ID == f.chosen("credential") {
					next.secret = s
				}
			}
			seconds, _ := strconv.ParseInt(f.chosen("duration"), 10, 64)
			next.ttl = time.Duration(seconds) * time.Second
			if f.chosen("duration") == "custom" {
				next.ttl, _ = lifetime.Parse(f.value("customDuration"))
			}
		}
		if delegate {
			next.delegateScope = f.chosen("scope")
		} else {
			next.useScope = f.chosen("scope")
			next.useAs = bindingplan.As(f.chosen("as"))
			next.raw = f.chosen("delivery")
			if next.raw == "sentinel" {
				next.raw = ""
			}
		}
		return next
	}
	d = approvalDialog(title, f, func(f *form) tea.Cmd {
		next := read()
		if d.formActions[d.formActionAt].key == "raw" {
			return m.showRawRequest(a.req, func() tea.Cmd { m.dialog = d; return nil })
		}
		answer := PartApprove
		if d.formActions[d.formActionAt].key == "deny" {
			answer = PartDeny
		}
		if answer == PartApprove {
			if f.chosen("duration") == "custom" {
				if _, err := lifetime.Parse(f.value("customDuration")); err != nil {
					f.err = err.Error()
					return nil
				}
			}
			if f.chosen("scope") == "hosts" && len(hosts) == 0 {
				f.err = "No open requested hosts; choose Requested routes or Anywhere."
				return nil
			}
			if !next.fresh && !withinLimit(next.secret, next.ttl) {
				f.err = "Credential allows at most " + lifetime.Label(next.secret.MaxTTL)
				return nil
			}
			if !delegate && next.raw != "" && (next.fresh || !next.secret.AllowRaw || next.useAs == bindingplan.Add) {
				f.err = "Raw delivery requires an existing credential that allows it and a replacement grant."
				return nil
			}
			if !delegate && next.useAs == bindingplan.Add && !next.canAdd() {
				f.err = variableOf(a.req) + " holds " + heldSecret(a.secrets, holder).Name + "; an addition stacks only on that credential. Choose it, or grant a replacement."
				return nil
			}
		}
		if delegate {
			next.delegate = answer
		} else {
			next.use = answer
			if answer == PartDeny {
				next.raw = ""
			}
		}
		if !delegate && a.req.Delegate != nil {
			next.partsChosen = true
			return m.approvalPart(next, true)
		}
		next.partsChosen = true
		if next.use != PartApprove && next.delegate != PartApprove {
			return m.denyCredential(a.req)
		}
		if next.fresh {
			return m.askNewCredentialForm(next)
		}
		return m.chooseSecret(next, next.secret.ID)
	})
	if a.req.Raw != "" {
		d.formActions = append(d.formActions, action{key: "raw", label: "Request JSON", enabled: true})
	}
	d.titleRight = "1 of 1"
	if a.req.asksBoth() {
		d.titleRight = "1 of 2"
		if delegate {
			d.titleRight = "2 of 2"
		}
	}
	d.refreshSections = func() {
		next := read()
		d.sections = []section{{fields: []field{{label: "Box", value: a.req.SandboxID, tone: toneAccent}, {label: "Reason (box's words)", value: a.req.Justification}}}}
		if a.req.Server != "" {
			d.sections[0].fields = append(d.sections[0].fields, field{label: "Server", value: a.req.Server})
		}
		if !delegate {
			d.sections[0].fields = append(d.sections[0].fields, field{label: "Variable", value: variableOf(a.req)})
		}
		explain := "This box may use the credential."
		if delegate {
			explain = "This box may give access to boxes it leads; delegation grants no use to this box."
		}
		d.sections = append(d.sections, section{lines: []line{{text: explain}}})
		if pinned {
			d.sections = append(d.sections, section{lines: []line{{text: variableOf(a.req) + " already holds " + heldSecret(a.secrets, holder).Name + " in this box; no other credential can be granted there, so it answers this.", tone: toneAccent}}})
		}
		if delegate && a.req.Use != nil {
			state := "Use denied. Credential and duration remain editable."
			if locked {
				state = "Use accepted for this answer. Credential and duration are locked. Nothing has been sent yet."
			}
			d.sections = append(d.sections, section{lines: []line{{text: state, tone: toneAccent}}})
		}
		d.sections = append(d.sections, approvalGrantSection(f.chosen("scope"), part, hosts))
		for _, role := range part.Roles {
			d.sections = append(d.sections, section{label: "Requested role (box's words)", lines: roleLines(role)})
		}
		limits := section{label: "Credential and parent ceilings", lines: []line{{text: "Every independent restriction still applies. Approval cannot widen a parent delegation."}}}
		if next.fresh {
			limits.lines = append(limits.lines, line{text: "Credential restrictions and lifetime limit are chosen on the new credential form."})
		} else {
			limits.fields = append(limits.fields, field{label: next.secret.Name, value: routeRows(next.secret.Routes)})
			for i, list := range next.secret.RouteConstraints {
				limits.fields = append(limits.fields, field{label: fmt.Sprintf("Constraint %d", i+1), value: routeRows(list)})
			}
			if next.secret.MaxTTL > 0 {
				limits.fields = append(limits.fields, field{label: "Maximum duration", value: lifetime.Label(next.secret.MaxTTL)})
			}
		}
		d.sections = append(d.sections, limits)
		if !delegate {
			effect := "Replace existing use grants in " + variableOf(a.req) + " for " + lifetime.Label(next.ttl) + ". The final review shows each affected grant."
			if next.useAs == bindingplan.Add {
				effect = "Add these routes for " + lifetime.Label(next.ttl) + ". Existing grants keep their routes and lifetimes."
			}
			d.sections = append(d.sections, section{label: "What accepting changes", fields: []field{{label: "Variable", value: variableOf(a.req)}}, lines: []line{{text: effect}, {text: rawDeliveryText(next.raw), tone: toneAlert}}})
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
		value := routeRows(parent.Routes)
		if len(parent.Routes) == 0 {
			value = "No additional parent route restriction"
		}
		d.sections = append(d.sections, section{label: "Parent delegation ceiling", fields: []field{{label: parent.ID, value: value}, {label: "Expires", value: parent.Expires.String()}}})
	}
	if a.use == PartApprove {
		d.sections = append(d.sections, heldSection(a))
	}
	d.sections = append(d.sections, section{lines: []line{{text: "This submits the use and delegation decisions together. Deny refuses the entire request; Esc leaves it waiting."}}})

	m.dialog = d
	return nil
}
