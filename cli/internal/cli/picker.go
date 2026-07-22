package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// errPickCanceled reports that the user dismissed a picker without choosing.
var errPickCanceled = errors.New("canceled")

// selectSandbox resolves the sandbox a command acts on. A sandbox given on the
// command line wins; otherwise the candidates are the sandboxes `disco ls`
// shows for the current project directory, and the user picks one when there is
// more than one.
func (a *App) selectSandbox(cmd *cobra.Command, sandboxArg string) (projectID string, sandboxID string, client *apiclientgen.Client, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return "", "", nil, err
	}
	client, err = a.apiClient()
	if err != nil {
		return "", "", nil, err
	}
	if strings.TrimSpace(sandboxArg) != "" {
		sandboxID, err = a.resolveSandboxID(cmd.Context(), client, projectID, sandboxArg)
		return projectID, sandboxID, client, err
	}
	sandboxes, err := a.listProjectSandboxes(cmd.Context(), client, projectID, false)
	if err != nil {
		return "", "", nil, err
	}
	sandboxID, err = pickOne(cmd, "Select a sandbox", sandboxPickerItems(sandboxes), pickerLabels{
		empty:     "no sandboxes were started from this directory; start one with `disco run`, or name one with --sandbox-id",
		ambiguous: "more than one sandbox was started from this directory; pass --sandbox-id",
	})
	return projectID, sandboxID, client, err
}

func sandboxPickerItems(sandboxes []apimodel.Sandbox) []pickerItem {
	sandboxes = sortedByCreatedAt(sandboxes, func(sandbox apimodel.Sandbox) time.Time { return sandbox.CreatedAt })
	items := make([]pickerItem, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		items = append(items, pickerItem{
			id:     sandbox.ID,
			title:  sandbox.Config.Name,
			detail: fmt.Sprintf("%s · %s", sandboxDisplayState(sandbox), formatTime(sandbox.UpdatedAt)),
		})
	}
	return items
}

// pickerItem is one choice: its resource ID plus what to show for it.
type pickerItem struct {
	id     string
	title  string
	detail string
}

// pickerLabels carries the caller's wording for the two cases the picker cannot
// resolve on its own, so each resource explains itself in its own terms.
type pickerLabels struct {
	// empty is the error when there is nothing to choose from.
	empty string
	// ambiguous is the error when there are several choices but no terminal to
	// ask on.
	ambiguous string
}

// pickOne returns the single item's ID when there is exactly one, and otherwise
// asks the user to choose. It prompts on stderr so a chosen-then-streamed
// command keeps stdout clean.
func pickOne(cmd *cobra.Command, prompt string, items []pickerItem, labels pickerLabels) (string, error) {
	switch len(items) {
	case 0:
		return "", errors.New(labels.empty)
	case 1:
		return items[0].id, nil
	}
	if !isTerminalStream(cmd.InOrStdin()) || !isTerminalStream(cmd.ErrOrStderr()) {
		return "", errors.New(labels.ambiguous)
	}
	model := &pickerModel{prompt: prompt, items: items, chosen: -1}
	final, err := tea.NewProgram(model,
		tea.WithContext(cmd.Context()),
		tea.WithInput(cmd.InOrStdin()),
		tea.WithOutput(cmd.ErrOrStderr()),
	).Run()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", errPickCanceled
		}
		return "", err
	}
	picked, ok := final.(*pickerModel)
	if !ok || picked.chosen < 0 {
		return "", errPickCanceled
	}
	return picked.items[picked.chosen].id, nil
}

func isTerminalStream(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

type pickerModel struct {
	prompt string
	items  []pickerItem
	cursor int
	// chosen is the selected index, or -1 while nothing is selected and after a
	// cancel.
	chosen int
	done   bool
}

func (m *pickerModel) Init() tea.Cmd { return nil }

func (m *pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "q", "esc", "ctrl+c":
		m.done = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.items)-1 {
			m.cursor++
		}
	case "enter":
		m.chosen = m.cursor
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

var (
	pickerTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	pickerCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	pickerDetailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	pickerHelpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

func (m *pickerModel) View() tea.View {
	if m.done {
		// Take the menu back down: the picked resource is the command's subject,
		// not part of its output.
		return tea.NewView("")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", pickerTitleStyle.Render(m.prompt))
	for i, item := range m.items {
		cursor := "  "
		line := fmt.Sprintf("%-24s %-20s", item.id, item.title)
		if i == m.cursor {
			cursor = pickerCursorStyle.Render("▸ ")
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		fmt.Fprintf(&b, "%s%s %s\n", cursor, line, pickerDetailStyle.Render(item.detail))
	}
	fmt.Fprintf(&b, "\n%s\n", pickerHelpStyle.Render("↑/↓ move · enter select · q/esc cancel"))
	return tea.NewView(b.String())
}
