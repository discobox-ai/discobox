//go:build prototype

package tui

import tea "charm.land/bubbletea/v2"

// API access has no inbox request kind today. CURRENT is the real Settings
// screen; PROPOSED explores the permission card a dedicated request would need.
func (p *approvalPreview) openAPI() {
	if p.current {
		box := Sandbox{ID: "sbx_manager", Name: "Manager", State: StateRunning}
		p.d = settingsDialog(box, BoxSettings{Name: "manager", HarnessID: "codex", DiscoboxAPI: p.readOnly}, []Harness{{ID: "codex", Name: "Codex", Slug: "codex"}})
		p.d.form.moveTo(len(p.d.form.rows) - 1)
		p.d.submit = func(*form) tea.Cmd { return p.receipt("API settings · sample saved") }
		return
	}
	var d *dialog
	title := "Allow Manager to use the discobox API?"
	if p.readOnly {
		title = "Turn off Manager's discobox API access?"
	}
	d = formDialog(title, newForm(), func(*form) tea.Cmd {
		result := d.formActions[d.formActionAt].key
		return p.receipt("Discobox API · " + result)
	})
	state := "Off"
	if p.readOnly {
		state = "On"
	}
	d.sections = []section{{fields: []field{
		{label: "Box", value: "Manager · sbx_manager", tone: toneAccent},
		{label: "Project", value: "discobox-ai"},
		{label: "Current access", value: state},
	}}}
	if p.readOnly {
		d.sections = append(d.sections, section{label: "What turning it off changes", lines: []line{
			{text: "Manager's calls to the discobox API will be refused.", tone: toneAccent},
			{text: "It can no longer create boxes or manage the boxes it leads."},
			{text: "Existing boxes keep running. Their parent relationships and credential grants remain."},
		}})
		d.formActions = []action{{key: "disable", label: "Turn off", enabled: true}, {key: "keep", label: "Keep on", enabled: true}}
	} else {
		d.sections[0].fields = append(d.sections[0].fields, field{label: "Reason", value: "“Create workers and coordinate their tasks in this project.”"})
		d.sections = append(d.sections, section{label: "Project access", fields: []field{
			{label: "Discovery", value: "List boxes and watch project events"},
			{label: "Create", value: "Launch new boxes within its allowed egress"},
		}}, section{label: "Authority over boxes it leads", fields: []field{
			{label: "Terminals", value: "Read output and type commands"},
			{label: "Lifecycle", value: "Start, stop, restart, repair, upgrade and archive"},
			{label: "Source/meta", value: "Deliver source and edit metadata"},
			{label: "Services/ports", value: "Inspect/control services and reach HTTP ports"},
			{label: "Credentials", value: "Answer eligible requests within its delegation"},
		}}, section{label: "Limits", lines: []line{
			{text: "Management is limited to boxes it leads in this project. Wider-egress children are read-only."},
			{text: "No credential is granted by enabling API access. Existing credential and egress restrictions still apply."},
			{text: "Access stays on until you turn it off in Settings."},
		}})
		d.formActions = []action{{key: "accept", label: "Accept", enabled: true}, {key: "deny", label: "Deny", enabled: true}}
	}
	d.formActionAt = 0
	d.keys = []hint{says("Tab / arrows choose"), pressing("enter selects", "enter"), pressing("esc leaves waiting", "esc")}
	d.footer = "Design proposal · API access currently uses Settings · no API call is made"
	p.d = d
}
