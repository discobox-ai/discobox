package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
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
	sandboxID, err = pickOne(cmd, "Select a sandbox", sandboxPickerItems(sandboxes), pickerOptions{
		empty:     "no sandboxes were started from this directory; start one with `disco run`, or name one with --sandbox-id",
		ambiguous: "more than one sandbox was started from this directory; pass --sandbox-id",
		// The remembered pick is per project, because the candidate list is.
		recentKey: "sandbox:" + projectID,
	})
	return projectID, sandboxID, client, err
}

func sandboxPickerItems(sandboxes []apimodel.Sandbox) []pickerItem {
	items := make([]pickerItem, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		updatedAt := recencyTime(sandbox.UpdatedAt, sandbox.CreatedAt)
		items = append(items, pickerItem{
			id:        sandbox.ID,
			title:     sandbox.Config.Name,
			detail:    fmt.Sprintf("%s · %s", sandboxDisplayState(sandbox), formatTime(updatedAt)),
			updatedAt: updatedAt,
		})
	}
	return items
}

// pickerItem is one choice: its resource ID plus what to show for it.
type pickerItem struct {
	id     string
	title  string
	detail string
	// updatedAt orders the list when the query does not: most recently touched
	// first, both unfiltered and among items the query scores equally.
	updatedAt time.Time
}

// pickerOptions carries what only the calling resource knows: its wording for
// the two cases the picker cannot resolve on its own, and the key its picks are
// remembered under.
type pickerOptions struct {
	// empty is the error when there is nothing to choose from.
	empty string
	// ambiguous is the error when there are several choices but no terminal to
	// ask on.
	ambiguous string
	// recentKey namespaces the remembered last pick. Empty disables the memory.
	recentKey string
}

// pickOne returns the single item's ID when there is exactly one, and otherwise
// asks the user to choose. It prompts on stderr so a chosen-then-streamed
// command keeps stdout clean.
func pickOne(cmd *cobra.Command, prompt string, items []pickerItem, opts pickerOptions) (string, error) {
	switch len(items) {
	case 0:
		return "", errors.New(opts.empty)
	case 1:
		return items[0].id, nil
	}
	if !isTerminalStream(cmd.InOrStdin()) || !isTerminalStream(cmd.ErrOrStderr()) {
		return "", errors.New(opts.ambiguous)
	}
	model := newPickerModel(prompt, items, lastSelection(opts.recentKey))
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
	id := picked.items[picked.chosen].id
	// Best-effort: an unwritable state directory must not fail the command.
	_ = rememberSelection(opts.recentKey, id)
	return id, nil
}

func isTerminalStream(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// pickerMatch is one item that survived the current query, with the rune
// offsets the query matched in each field so the view can highlight them.
type pickerMatch struct {
	item        pickerItem
	index       int
	score       int
	idPos       []int
	titlePos    []int
	detailPos   []int
	matchedAnyF bool
	// recent marks the last pick, which leads the list while there is no query.
	recent bool
}

// pickerVisible caps how many rows the picker draws at once; the window follows
// the cursor so a long list stays readable.
const pickerVisible = 20

type pickerModel struct {
	prompt string
	items  []pickerItem
	// query is what the user has typed; it fuzzy-filters and ranks items.
	query string
	// recentID is the ID picked last time, which leads the unfiltered list. Once
	// a query is typed the query alone decides the order.
	recentID string
	matches  []pickerMatch
	cursor   int
	offset   int
	// chosen is the index into items of the selection, or -1 while nothing is
	// selected and after a cancel.
	chosen int
	done   bool
}

func newPickerModel(prompt string, items []pickerItem, recentID string) *pickerModel {
	m := &pickerModel{prompt: prompt, items: items, recentID: recentID, chosen: -1}
	m.refilter()
	return m
}

func (m *pickerModel) Init() tea.Cmd { return nil }

func (m *pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "ctrl+c":
		m.done = true
		return m, tea.Quit
	case "esc":
		// Backing out of a query first keeps a typo from canceling the command.
		if m.query != "" {
			m.query = ""
			m.refilter()
			return m, nil
		}
		m.done = true
		return m, tea.Quit
	case "up", "ctrl+p":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "ctrl+n":
		if m.cursor < len(m.matches)-1 {
			m.cursor++
		}
	case "backspace":
		if q := []rune(m.query); len(q) > 0 {
			m.query = string(q[:len(q)-1])
			m.refilter()
		}
	case "ctrl+u":
		m.query = ""
		m.refilter()
	case "enter":
		if len(m.matches) == 0 {
			return m, nil
		}
		m.chosen = m.matches[m.cursor].index
		m.done = true
		return m, tea.Quit
	default:
		// Anything printable extends the query; modifiers are reserved for the
		// bindings above.
		if key.Mod&(tea.ModCtrl|tea.ModAlt) == 0 && key.Text != "" {
			m.query += key.Text
			m.refilter()
		}
	}
	m.scrollToCursor()
	return m, nil
}

// refilter recomputes the visible matches for the current query, keeping the
// item the cursor was on selected when it survives.
func (m *pickerModel) refilter() {
	var selected = -1
	if m.cursor < len(m.matches) {
		selected = m.matches[m.cursor].index
	}
	m.matches = fuzzyPickerMatches(m.items, m.query, m.recentID)
	m.cursor = 0
	for i, match := range m.matches {
		if match.index == selected {
			m.cursor = i
			break
		}
	}
	m.scrollToCursor()
}

func (m *pickerModel) scrollToCursor() {
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+pickerVisible {
		m.offset = m.cursor - pickerVisible + 1
	}
	if last := len(m.matches) - pickerVisible; m.offset > last {
		m.offset = last
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// Field weights break ties toward the field a user most likely typed at: the
// name they gave the sandbox, then its ID, then incidental detail text.
const (
	pickerTitleWeight  = 30
	pickerIDWeight     = 10
	pickerDetailWeight = 0
)

// fuzzyPickerMatches ranks items best-first for the query, falling back to most
// recently updated first when the query is empty or scores items equally. With
// no query, recentID leads: it is the pick the user made here last, and typing
// anything at all is what says they mean something else this time.
func fuzzyPickerMatches(items []pickerItem, query string, recentID string) []pickerMatch {
	unfiltered := strings.TrimSpace(query) == ""
	matches := make([]pickerMatch, 0, len(items))
	for i, item := range items {
		match := pickerMatch{item: item, index: i, score: math.MinInt}
		if unfiltered {
			matches = append(matches, pickerMatch{item: item, index: i, recent: item.id == recentID && recentID != ""})
			continue
		}
		for _, field := range []struct {
			text      string
			weight    int
			positions *[]int
		}{
			{item.title, pickerTitleWeight, &match.titlePos},
			{item.id, pickerIDWeight, &match.idPos},
			{item.detail, pickerDetailWeight, &match.detailPos},
		} {
			score, positions, ok := fuzzyMatch(field.text, query)
			if !ok {
				continue
			}
			*field.positions = positions
			match.matchedAnyF = true
			if score+field.weight > match.score {
				match.score = score + field.weight
			}
		}
		if match.matchedAnyF {
			matches = append(matches, match)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].recent != matches[j].recent {
			return matches[i].recent
		}
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[j].item.updatedAt.Before(matches[i].item.updatedAt)
	})
	return matches
}

var (
	pickerTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	pickerCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	pickerDetailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	pickerHelpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	pickerQueryStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	pickerMatchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	pickerRecentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

func (m *pickerModel) View() tea.View {
	if m.done {
		// Take the menu back down: the picked resource is the command's subject,
		// not part of its output.
		return tea.NewView("")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", pickerTitleStyle.Render(m.prompt))
	fmt.Fprintf(&b, "%s%s\n\n", pickerQueryStyle.Render("search: "), m.query+"▏")
	if len(m.matches) == 0 {
		fmt.Fprintf(&b, "%s\n", pickerDetailStyle.Render("  no matches"))
	}
	end := min(m.offset+pickerVisible, len(m.matches))
	for i := m.offset; i < end; i++ {
		match := m.matches[i]
		selected := i == m.cursor
		cursor := "  "
		if selected {
			cursor = pickerCursorStyle.Render("▸ ")
		}
		detail := renderPickerField(match.item.detail, match.detailPos, 0, pickerDetailStyle, selected)
		if match.recent {
			// Say why this row is on top, so leading with it does not look arbitrary.
			detail += pickerRecentStyle.Render(" · last used")
		}
		fmt.Fprintf(&b, "%s%s %s %s\n", cursor,
			renderPickerField(match.item.id, match.idPos, 24, lipgloss.NewStyle(), selected),
			renderPickerField(match.item.title, match.titlePos, 20, lipgloss.NewStyle(), selected),
			detail)
	}
	if hidden := len(m.matches) - end; hidden > 0 {
		fmt.Fprintf(&b, "%s\n", pickerDetailStyle.Render(fmt.Sprintf("  … %d more", hidden)))
	}
	fmt.Fprintf(&b, "\n%s\n", pickerHelpStyle.Render("type to search · ↑/↓ move · enter select · esc clear/cancel"))
	return tea.NewView(b.String())
}

// renderPickerField draws one column, highlighting the runes the query matched
// and padding to width so columns line up despite the styling.
func renderPickerField(text string, positions []int, width int, base lipgloss.Style, selected bool) string {
	if selected {
		base = base.Bold(true)
	}
	matched := make(map[int]bool, len(positions))
	for _, pos := range positions {
		matched[pos] = true
	}
	var b strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && matched[j] == matched[i] {
			j++
		}
		style := base
		if matched[i] {
			style = pickerMatchStyle
			if selected {
				style = style.Bold(true)
			}
		}
		b.WriteString(style.Render(string(runes[i:j])))
		i = j
	}
	if pad := width - len(runes); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}
