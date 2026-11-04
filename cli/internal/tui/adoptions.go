package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) askAboutAdoption(req CredentialRequest) tea.Cmd {
	ask := req.Adoption
	d := actionsDialog("Adoption request", "", []action{
		{key: "approve", label: "Approve adoption", enabled: true},
		{key: "deny", press: "d", label: "Deny", enabled: true},
	}, func(result string) tea.Cmd { return m.answerAdoption(req, result == "approve") })
	d.sections = []section{{label: "who leads this discobox", fields: []field{
		{label: "child", value: fmt.Sprintf("%s (%s)", ask.ChildName, ask.ChildID), tone: toneAccent},
		{label: "requester", value: fmt.Sprintf("%s (%s)", ask.RequesterName, ask.RequesterID)},
		{label: "current parent", value: ask.CurrentParentID},
		{label: "reason (the discobox's words)", value: req.Justification},
	}}, {label: "approval allows the parent to", lines: []line{
		{text: "Read and type into terminals; start, stop, repair, upgrade and archive the child; change its meta; deliver its source; reach its ports; answer credential requests within what the parent may delegate."},
		{text: "The child's credentials stay its own. Authority covers this child alone."},
	}}}
	if ask.ReadOnly {
		d.sections = append(d.sections, section{label: "read access only", lines: []line{{text: "The child has wider egress. Every write and send into it will be refused; this parent may only read it.", tone: toneAlert}}})
	}
	d.footer = "Approval replaces the current parent. Esc leaves the request waiting."
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
