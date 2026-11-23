package tui

import (
	"strings"

	"github.com/discobox-ai/discobox/pkg/bindingplan"
)

// rebindPreview is what approving a request with another secret than the
// one the box's own binding holds its variable with does (ADR 26-11-19-927):
// the variable moves to the chosen secret, the binding holding it now loses
// its use, keeping a delegation it also holds, the old secret's additions
// there go, and the variable keeps its sentinel unless the two secrets'
// sentinels have different shapes. The approval card shows it before the
// person accepts, and the approval names the binding they saw
// (Approval.Replaces).
// Which binding loses what is bindingplan.Rebind, the server's rule.
type rebindPreview struct {
	// holder is the grant delivering the variable now; from is its secret.
	holder HeldGrant
	from   Secret
	// binding is the box's own binding holder stands for, read with the
	// box's bindings (read); found is false when it was not among them.
	binding SecretBinding
	found   bool
	plan    bindingplan.Plan
	byID    map[string]SecretBinding
	envVar  string
}

// read fills the preview from bindings, the box's bindings as the approval
// card reads them; the old secret's are its own.
func (r *rebindPreview) read(bindings []SecretBinding, req CredentialRequest) {
	r.byID, r.envVar = map[string]SecretBinding{}, variableOf(req)
	var mine []SecretBinding
	for _, b := range bindings {
		if b.SecretID != r.holder.SecretID || b.TargetKind != BindingSandbox || b.TargetID != req.SandboxID {
			continue
		}
		r.byID[b.ID] = b
		mine = append(mine, b)
		if b.ID == r.holder.BindingID {
			r.binding, r.found = b, true
		}
	}
	r.plan = bindingplan.Rebind(planBindings(mine), r.holder.BindingID, req.EnvVar)
}

// unavailable is why a person's approval cannot rebind the variable, by the
// rule the server refuses it by (bindingplan.ApprovalRefusal), asked of every
// binding the rebind touches: one handed on under a delegation, or one
// handing over the value itself, is rebound directly instead, and the card
// says how. ans is the answer with the chosen secret.
func (r *rebindPreview) unavailable(req CredentialRequest, ans requestAnswer) string {
	why := ""
	if !r.found {
		why = "the binding holding it, " + r.holder.BindingID + ", could not be read"
	}
	for _, step := range r.plan.Steps {
		if refusal := bindingplan.ApprovalRefusal(step.Binding); why == "" && refusal != "" {
			why = "binding " + step.Binding.ID + " there is " + string(refusal) + ", which an approval never takes over"
		}
	}
	if why == "" {
		return ""
	}
	return "Approving with “" + ans.credential + "” would rebind " + r.envVar + " from “" + r.from.Name + "”, but " + why + ". " +
		"Answer with “" + r.from.Name + "”, or rebind it directly: " +
		strings.Join([]string{"discobox admin binding create --secret", ans.secret.Name, "--target-kind sandbox --target", req.SandboxID, "--as", r.envVar, "--replaces", r.holder.BindingID}, " ") + "."
}

// sections are the card's account of the rebind: both secrets and their
// routes, what the replaced binding loses and keeps, and the sentinel.
func (r *rebindPreview) sections(ans requestAnswer) []section {
	variable := r.envVar
	head := section{label: "this rebinds " + variable, labelTone: toneAlert, lines: []line{
		{text: variable + " moves from “" + r.from.Name + "” to “" + ans.credential + "”", tone: toneAlert},
	}}
	for _, step := range r.plan.Steps {
		b := r.byID[step.Binding.ID]
		switch {
		case step.Binding.Adds:
			head.lines = append(head.lines, line{text: "the addition " + b.ID + " of “" + r.from.Name + "” — goes", tone: toneAlert, bullet: true})
		case step.Fate == bindingplan.LosesUse:
			head.lines = append(head.lines, line{text: b.ID + " — loses its use in " + variable, tone: toneAlert, bullet: true})
		default:
			head.lines = append(head.lines, line{text: b.ID + " — goes", tone: toneAlert, bullet: true})
		}
	}
	switch {
	case !r.binding.Delegate:
		head.lines = append(head.lines, line{text: "keeps nothing: " + r.binding.ID + " only used “" + r.from.Name + "”", tone: toneDim})
	default:
		head.lines = append(head.lines, line{text: "keeps its delegation of “" + r.from.Name + "”, and what was handed on under it"})
	}
	// The old secret's format is unknown when this window cannot read it;
	// then neither outcome is promised.
	switch {
	case r.from.Format == "":
		head.lines = append(head.lines, line{text: "the sentinel is kept only if both credentials share a shape; otherwise processes holding " + variable + " must restart", tone: toneAlert})
	case bindingplan.KeepsSentinel(r.from.Format, ans.secret.Format):
		head.lines = append(head.lines, line{text: "sentinel kept: running processes switch to “" + ans.credential + "”"})
	default:
		head.lines = append(head.lines, line{text: "a new sentinel, since “" + ans.credential + "” has another shape: processes holding " + variable + " must restart", tone: toneAlert})
	}
	out := []section{head}
	if len(r.binding.Routes) > 0 {
		out = append(out, routesSection(r.binding.ID+" was held to", r.binding.Routes))
	}
	return append(out,
		routesSection("“"+r.from.Name+"” may be sent to", r.from.Routes),
		routesSection("“"+ans.credential+"” may be sent to", ans.secret.Routes))
}
