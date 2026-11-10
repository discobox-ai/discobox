package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// askAboutAdoption is an adoption request's card (console.AdoptionCard),
// accepted or denied.
func (m *Model) askAboutAdoption(req console.CredentialRequest) tea.Cmd {
	var d *dialog
	d = approvalDialog(console.AdoptionTitle(req), newForm(), console.CardActions(req, false), func(*form) tea.Cmd { return m.answerAccess(req, d.chosenAction().Accepts()) })
	d.sections = detailSections(console.AdoptionCard(req))
	m.dialog = d
	return nil
}
