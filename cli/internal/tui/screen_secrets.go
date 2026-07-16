package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// secretsScreen is a placeholder for the project secrets tab. The secrets
// backend does not exist yet, so it renders a centered "coming soon" panel and
// only wires up the shared chrome keys (quit, help) plus Up to surface focus
// back to the tab bar. It is a full peer of the sandboxes and agents tabs so the
// tab navigation has a real third destination to land on.
type secretsScreen struct {
	keys   keyMap
	styles styles

	width  int
	height int
}

func newSecretsScreen(keys keyMap, st styles) *secretsScreen {
	return &secretsScreen{keys: keys, styles: st}
}

func (s *secretsScreen) Init() tea.Cmd { return nil }

func (s *secretsScreen) Update(msg tea.Msg) (screen, tea.Cmd) {
	switch msg := msg.(type) {
	case resizeMsg:
		s.width, s.height = msg.width, msg.height
		return s, nil
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, s.keys.Quit):
			return s, tea.Quit
		case key.Matches(msg, s.keys.Help):
			return s, func() tea.Msg { return toggleHelpMsg{} }
		case key.Matches(msg, s.keys.Up):
			return s, focusTabsCmd()
		}
	}
	return s, nil
}

func (s *secretsScreen) View(width, height int) string {
	s.width, s.height = width, height
	title := s.styles.paneTitle.Render("Secrets")
	body := s.styles.status.Render("coming soon — project secrets will live here")
	content := lipgloss.JoinVertical(lipgloss.Center, title, "", body)
	if width <= 0 || height <= 0 {
		return content
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}

func (s *secretsScreen) title() string { return "secrets" }

func (s *secretsScreen) helpBindings() []key.Binding {
	return []key.Binding{s.keys.Left, s.keys.Right, s.keys.Up, s.keys.Help, s.keys.Quit}
}

func (s *secretsScreen) fullHelpBindings() [][]key.Binding {
	return [][]key.Binding{{s.keys.Left, s.keys.Right, s.keys.Up}, {s.keys.Help, s.keys.Quit}}
}

func (s *secretsScreen) cursor(int, int) *tea.Cursor { return nil }
