package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) askAboutAdoption(req CredentialRequest) tea.Cmd {
	ask := req.Adoption
	child, requester := ask.ChildID, ask.RequesterID
	if ask.ChildName != "" {
		child = fmt.Sprintf("%s (%s)", ask.ChildName, ask.ChildID)
	}
	if ask.RequesterName != "" {
		requester = fmt.Sprintf("%s (%s)", ask.RequesterName, ask.RequesterID)
	}
	var d *dialog
	d = approvalDialog("Let "+requester+" lead "+child+"?", newForm(), func(*form) tea.Cmd { return m.answerAdoption(req, d.formActions[d.formActionAt].key == "accept") })
	d.sections = []section{{fields: []field{{label: "Requesting box", value: requester, tone: toneAccent}, {label: "Target box", value: child, tone: toneAccent}, {label: "Current parent", value: ask.CurrentParentID}, {label: "Reason (box's words)", value: req.Justification}}}}
	if ask.ReadOnly {
		d.sections = append(d.sections, section{label: "Read-only authority", lines: []line{{text: "The target has wider egress. The requesting box may only read it.", tone: toneAlert}, {text: "All writes and sends into it will be refused."}}})
	} else {
		d.sections = append(d.sections, section{label: "Authority over this box", fields: []field{{label: "Terminals/files", value: "Run commands; read and write files; attach to and end execs; read and type into terminals"}, {label: "Lifecycle", value: "Start, stop, restart, repair, upgrade and archive"}, {label: "Settings/source", value: "Change metadata and deliver its source"}, {label: "Services/ports", value: "Inspect and control services; reach ports"}, {label: "Credentials", value: "Answer requests within the parent's delegation"}}})
	}
	d.sections = append(d.sections, section{label: "What accepting changes", lines: []line{{text: requester + " replaces " + ask.CurrentParentID + " as this box's parent.", tone: toneAccent}, {text: "The child's credentials stay its own. Commands and file reads may expose existing files and credential sentinels. Authority covers this child alone."}, {text: "Deny keeps the current parent. Esc leaves the request waiting."}}})
	m.dialog = d
	return nil
}

func (m *Model) answerAdoption(req CredentialRequest, approve bool) tea.Cmd {
	m.dialog = statusDialog("Adoption request", "answering…")
	return func() tea.Msg {
		err := m.ds.DecideAdoptionRequest(m.ctx, req.Server, req.ID, approve)
		return credentialAnsweredMsg{request: req, approved: approve, err: err}
	}
}
