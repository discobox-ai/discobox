//go:build prototype

package tui

import tea "charm.land/bubbletea/v2"

// Adoption is a relationship decision, not a credential grant. The sample
// shows the same authority as askAboutAdoption without its long paragraph.
func (p *approvalPreview) openAdoption() {
	req := CredentialRequest{ID: "adopt_sample", SandboxID: "sbx_worker", Justification: "Take over this worker so I can finish the approval form redesign.", Adoption: &AdoptionAsk{ChildID: "sbx_worker", ChildName: "Approval forms", RequesterID: "sbx_manager", RequesterName: "Manager", CurrentParentID: "sbx_previous", ReadOnly: p.readOnly}}
	if p.current {
		m := &Model{}
		m.askAboutAdoption(req)
		p.d = m.dialog
		p.d.action = func(result string) tea.Cmd { return p.receipt("Adoption · " + result) }
		return
	}
	var d *dialog
	d = formDialog("Let Manager lead Approval forms?", newForm(), func(*form) tea.Cmd {
		return p.receipt("Adoption · " + d.formActions[d.formActionAt].key)
	})
	d.titleRight = "2m ago"
	d.sections = []section{
		{fields: []field{{label: "Requesting box", value: "Manager · sbx_manager", tone: toneAccent}, {label: "Target box", value: "Approval forms · sbx_worker", tone: toneAccent}, {label: "Current parent", value: "sbx_previous"}, {label: "Reason", value: "“" + req.Justification + "”"}}},
	}
	if p.readOnly {
		d.sections = append(d.sections, section{label: "Read-only authority", lines: []line{{text: "The target has wider egress. Manager may only read this box.", tone: toneAlert}, {text: "All writes and sends into it will be refused."}}})
	} else {
		d.sections = append(d.sections, section{label: "Authority over this box", fields: []field{
			{label: "Terminals", value: "Read output and type commands"},
			{label: "Lifecycle", value: "Start, stop, repair, upgrade and archive"},
			{label: "Settings/source", value: "Change metadata and deliver source"},
			{label: "Ports", value: "Reach this box's ports"},
			{label: "Credentials", value: "Answer requests within Manager's delegation"},
		}})
	}
	d.sections = append(d.sections, section{label: "What accepting changes", lines: []line{
		{text: "Manager replaces sbx_previous as this box's parent.", tone: toneAccent},
		{text: "This box's credentials stay its own. Authority covers this box alone."},
	}})
	d.formActions = []action{{key: "accept", label: "Accept", enabled: true}, {key: "deny", label: "Deny", enabled: true}}
	d.formActionAt = 0
	d.keys = []hint{says("Tab / arrows choose"), pressing("enter selects", "enter"), pressing("esc leaves waiting", "esc")}
	d.footer = "Sample only · Deny keeps the current parent · F3 compares read-only authority"
	p.d = d
}
