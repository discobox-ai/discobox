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

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// errPickCanceled reports that the user dismissed a picker without choosing.
var errPickCanceled = errors.New("canceled")

// selectSandbox resolves the sandbox a command acts on. A sandbox given on the
// command line wins; otherwise the candidates are the sandboxes `discobox ls`
// shows for the current project directory, and the user picks one when there is
// more than one. "a" in that picker widens the list to `discobox ls --all`, for
// the discobox that is in this project but was started somewhere else.
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
	sandboxes, err := a.listProjectSandboxCandidates(cmd.Context(), client, projectID, false)
	if err != nil {
		return "", "", nil, err
	}
	sandboxID, err = pickOne(cmd, "Select a discobox", sandboxPickerItems(sandboxes, ""), pickerOptions{
		empty:     "no discoboxes were started from this directory; start one with `discobox run`, or name one with --discobox-id",
		ambiguous: "more than one discobox was started from this directory; pass --discobox-id",
		// The remembered pick is per project, because the candidate list is.
		recentKey: "sandbox:" + projectID,
		expand:    a.sandboxPickerExpansion(cmd.Context(), client, projectID),
	})
	return projectID, sandboxID, client, err
}

// listProjectSandboxCandidates is the shared candidate list for commands that
// infer a sandbox from the current directory, or, with all, for every discobox
// in the project the way `discobox ls --all` lists them. Archived sandboxes
// remain in `discobox ls` and explicit resource completion so they can be
// inspected and unarchived, but they have no runtime and cannot be selected
// for an operation.
func (a *App) listProjectSandboxCandidates(ctx context.Context, client *apiclientgen.Client, projectID string, all bool) ([]apimodel.Sandbox, error) {
	sandboxes, err := a.listProjectSandboxes(ctx, client, projectID, all)
	if err != nil {
		return nil, err
	}
	candidates := make([]apimodel.Sandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if sandbox.Runtime.DesiredState == apiclientgen.SandboxRuntimeDesiredStatePresent {
			candidates = append(candidates, sandbox)
		}
	}
	return candidates, nil
}

// sandboxPickerExpansion is the wider list a sandbox picker offers behind "a":
// every discobox in the project, whatever directory or machine it was started
// from, which is exactly what `discobox ls --all` lists. Its rows carry where
// each discobox came from, because once the list crosses directories that is
// the only thing telling two identically named discoboxes apart.
func (a *App) sandboxPickerExpansion(ctx context.Context, client *apiclientgen.Client, projectID string) func() ([]pickerItem, error) {
	return func() ([]pickerItem, error) {
		localHostID, err := hostid.Get()
		if err != nil {
			return nil, err
		}
		sandboxes, err := a.listProjectSandboxCandidates(ctx, client, projectID, true)
		if err != nil {
			return nil, err
		}
		return sandboxPickerItems(sandboxes, localHostID), nil
	}
}

// sandboxPickerItems renders the picker rows for sandboxes. A row's detail is
// `discobox ls`'s answer in the same words: the runtime state, then whether the
// working tree holds work — dirty, ready, applied, clean — then when it was
// last touched. The git word is dropped rather than drawn as "-" when no agent
// has reported: an empty column reads as a column in a table, but as noise in a
// sentence.
//
// localHostID is empty for the current directory's list, where every row
// shares one origin and repeating it on each of them says nothing. The
// expanded list passes this machine's host ID instead, so each row can say
// where its discobox came from and name the machine when that is not this one.
func sandboxPickerItems(sandboxes []apimodel.Sandbox, localHostID string) []pickerItem {
	items := make([]pickerItem, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		updatedAt := recencyTime(sandbox.UpdatedAt, sandbox.CreatedAt)
		name := strings.TrimSpace(sandbox.DisplayName)
		if name == "" {
			name = strings.TrimSpace(sandbox.Config.Name)
		}
		if name == "" {
			name = sandbox.ID
		}
		detail := []string{sandboxDisplayState(sandbox)}
		if changes := sandboxGitStatus(sandbox).changes(sandboxSpawnCommit(sandbox)); changes != "-" {
			detail = append(detail, changes)
		}
		detail = append(detail, formatTime(updatedAt))
		item := pickerItem{
			id:        sandbox.ID,
			title:     name,
			detail:    strings.Join(detail, " · "),
			updatedAt: updatedAt,
		}
		if localHostID != "" {
			item.origin, item.originHost = sandboxPickerOrigin(sandbox, localHostID)
		}
		items = append(items, item)
	}
	return items
}

// sandboxPickerOrigin says where a discobox came from: its source — the
// directory or repository URL it was cut from, or that it had none — and the
// machine when that machine is not this one. It is the picker's reading of the
// SOURCE column `discobox ls --all` adds for the same reason. The two are kept
// apart rather than pasted together because only the path may be shortened —
// and because a Windows path carries a colon of its own, so nothing downstream
// could take them back apart.
func sandboxPickerOrigin(sandbox apimodel.Sandbox, localHostID string) (path, host string) {
	origin, ok := sandbox.Origin.Get()
	if !ok {
		return "", ""
	}
	path = sandboxSource(sandbox)
	if path == "-" {
		path = "no source"
	}
	if origin.HostId == localHostID {
		return path, ""
	}
	host = strings.TrimSpace(origin.Hostname.Or(""))
	if host == "" {
		host = "another machine"
	}
	return path, host
}

// pickerItem is one choice: its resource ID plus what to show for it.
type pickerItem struct {
	id     string
	title  string
	detail string
	// origin says where the item's resource lives — for a sandbox, the
	// directory it was started from. It is a trailing column drawn only when
	// the terminal leaves room for it, and shortened from its front, a path
	// being identified by its last components rather than its first.
	origin string
	// originHost names the machine origin sits on, when that is not this one.
	// Shortening keeps it whole and takes the room out of the path instead: it
	// is the reason the row can be told from a local one at all.
	originHost string
	// updatedAt orders the list when the query does not: most recently touched
	// first, both unfiltered and among items the query scores equally.
	updatedAt time.Time
}

// originText is the origin as one string, which is both what a row draws when
// it fits whole and what a query is matched against.
func (i pickerItem) originText() string {
	switch {
	case i.origin == "":
		return ""
	case i.originHost == "":
		return i.origin
	default:
		return i.originHost + ":" + i.origin
	}
}

// The origin column's limits. pickerOriginWidth is as wide as it may get
// however roomy the terminal — past that it crowds out nothing useful and only
// makes the card harder to read across. pickerOriginMin is the least it is
// worth drawing at: below that it is more ellipsis than path, so a narrow
// terminal gets no origin column rather than an unreadable one.
// pickerOriginMinPath is what the path keeps when a machine name is competing
// with it for the same budget.
const (
	pickerOriginWidth   = 44
	pickerOriginMin     = 16
	pickerOriginMinPath = 10
)

// shortenPickerOrigin draws item's origin in budget runes, and reports whether
// it had to shorten it to get there. A shortened origin is drawn without match
// highlighting: the offsets a query produced were measured against the whole
// string, and against a shortened one they would light up the wrong runes.
func shortenPickerOrigin(item pickerItem, budget int) (string, bool) {
	full := item.originText()
	if full == "" || budget <= 0 {
		return "", false
	}
	if lipgloss.Width(full) <= budget {
		return full, false
	}
	prefix := ""
	if item.originHost != "" {
		prefix = item.originHost + ":"
		if budget-lipgloss.Width(prefix) < pickerOriginMinPath {
			// Neither fits whole. The machine keeps its front, which is where
			// one hostname differs from another, and the path keeps its end.
			host, _ := truncatePickerText(item.originHost, max(budget-pickerOriginMinPath-1, 1))
			prefix = host + ":"
		}
	}
	room := budget - lipgloss.Width(prefix)
	if room < 1 {
		return prefix, true
	}
	return prefix + elidePickerPath(item.origin, room), true
}

// positionsWithin drops the match offsets that a truncation cut away.
func positionsWithin(positions []int, limit int) []int {
	kept := make([]int, 0, len(positions))
	for _, pos := range positions {
		if pos < limit {
			kept = append(kept, pos)
		}
	}
	return kept
}

// Every width in this file is a count of terminal cells, never of runes. The
// two part on any wide rune — a CJK title is one rune to two cells — and a
// picker that decided in one unit and padded in the other would misalign its
// columns and outgrow the window exactly as an over-long title did. Cells are
// what the terminal has, so cells are the unit throughout: `lipgloss.Width`
// measures, and these two spend.

// truncatePickerText cuts text to width cells, keeping its front, and reports
// how many runes survived ahead of the ellipsis — which is the limit the match
// offsets measured against the whole string are still good for.
func truncatePickerText(text string, width int) (string, int) {
	if lipgloss.Width(text) <= width {
		return text, len([]rune(text))
	}
	if width <= 1 {
		return strings.Repeat("…", max(width, 0)), 0
	}
	var b strings.Builder
	kept, used := 0, 0
	for _, r := range text {
		cells := lipgloss.Width(string(r))
		if used+cells > width-1 {
			break
		}
		b.WriteRune(r)
		used, kept = used+cells, kept+1
	}
	return b.String() + "…", kept
}

// elidePickerPath keeps the end of path within width cells, a path being
// identified by its last components rather than its first.
func elidePickerPath(path string, width int) string {
	if lipgloss.Width(path) <= width {
		return path
	}
	if width <= 1 {
		return strings.Repeat("…", max(width, 0))
	}
	runes := []rune(path)
	used, i := 0, len(runes)
	for i > 0 {
		cells := lipgloss.Width(string(runes[i-1]))
		if used+cells > width-1 {
			break
		}
		used, i = used+cells, i-1
	}
	return "…" + string(runes[i:])
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
	// expand is the wider list the picker offers behind "a", the picker's
	// equivalent of `discobox ls --all`: every discobox in the project rather
	// than only the ones started from this directory on this machine. It is
	// called the first time the user asks for it and its answer kept for the
	// rest of the picker's life. Nil is a picker with nothing wider to show.
	expand func() ([]pickerItem, error)
	// live rewrites the prompt line while the picker is up, for a question
	// whose subject is still being worked out behind it — the size of a
	// directory being measured while it is asked about. It returns the line and
	// whether that is the last of it; the picker polls until it says so. Nil is
	// a prompt that never changes.
	live func() (string, bool)
}

// pickOne returns the single item's ID when there is exactly one, and otherwise
// asks the user to choose. It prompts on stderr so a chosen-then-streamed
// command keeps stdout clean.
//
// The one candidate is still taken without asking even when a wider list
// exists: the command was run somewhere that started exactly one, and stopping
// to ask about it would cost every such run a keystroke to answer a question
// with one answer. Nothing at all is different — there is no candidate to take
// and nothing to lose by asking — so a wider list is opened straight onto
// rather than reported as a dead end.
func pickOne(cmd *cobra.Command, prompt string, items []pickerItem, opts pickerOptions) (string, error) {
	askable := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.ErrOrStderr())
	switch {
	case len(items) == 1:
		return items[0].id, nil
	case len(items) == 0 && (opts.expand == nil || !askable):
		return "", errors.New(opts.empty)
	case len(items) > 1 && !askable:
		return "", errors.New(opts.ambiguous)
	}
	model := newPickerModel(prompt, items, lastSelection(opts.recentKey))
	model.live = opts.live
	model.expand = opts.expand
	model.expandAtOnce = len(items) == 0
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
	if !ok {
		return "", errPickCanceled
	}
	if picked.chosen < 0 {
		switch {
		case picked.expandAtOnce && picked.expandErr != nil:
			return "", picked.expandErr
		case picked.exhausted:
			return "", errors.New(opts.empty)
		}
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
	originPos   []int
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
	// typing is the explicit / search mode, matching the launcher's F1 help.
	// Outside it, letters remain navigation keys rather than silently opening a
	// text field the screen did not offer.
	typing bool
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
	// live is pickerOptions.live, polled on pickerLiveInterval until it reports
	// the prompt is final.
	live func() (string, bool)
	// expand is pickerOptions.expand. scoped is the list the picker opened on,
	// kept so "a" can switch back; wide is what expand answered, kept so
	// switching back and forth asks the server once. expanded says which of the
	// two items currently holds.
	expand   func() ([]pickerItem, error)
	scoped   []pickerItem
	wide     []pickerItem
	expanded bool
	// expanding is a load in flight; expandErr is the last one that failed,
	// reported in the card rather than taking the picker down — the scoped list
	// on screen is still a usable answer.
	expanding bool
	expandErr error
	// expandAtOnce opens the picker already widened, for a command whose own
	// directory started nothing: there is no scoped list to show, and the wider
	// one is the only answer that could exist.
	expandAtOnce bool
	// exhausted marks a pre-widened picker that found nothing anywhere, so the
	// caller reports its own "there are none" wording rather than a cancel.
	exhausted bool
	// width is the terminal's, from the runtime. Zero until it has said, which
	// is only ever before the first frame.
	width int
}

// pickerExpandedMsg carries the wider list back from the load "a" started.
type pickerExpandedMsg struct {
	items []pickerItem
	err   error
}

// pickerLiveInterval is how often a live prompt is re-read. It is a number
// climbing on screen, so it wants to be seen moving and not much more.
const pickerLiveInterval = 150 * time.Millisecond

// pickerLiveMsg is the poll of a live prompt coming due.
type pickerLiveMsg struct{}

func newPickerModel(prompt string, items []pickerItem, recentID string) *pickerModel {
	m := &pickerModel{prompt: prompt, items: items, scoped: items, recentID: recentID, chosen: -1}
	m.refilter()
	return m
}

func (m *pickerModel) Init() tea.Cmd {
	if m.expandAtOnce {
		return tea.Batch(m.pollLive(), m.toggleScope())
	}
	return m.pollLive()
}

func (m *pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case pickerLiveMsg:
		prompt, final := m.live()
		m.prompt = prompt
		if final {
			return m, nil
		}
		return m, m.pollLive()
	case pickerExpandedMsg:
		m.expanding = false
		if msg.err != nil {
			m.expandErr = msg.err
			if m.expandAtOnce {
				// The wider list was the whole picker; failing to load it
				// leaves an empty card with nothing to pick.
				m.done = true
				return m, tea.Quit
			}
			return m, nil
		}
		// Normalized so an empty project reads as loaded rather than as never
		// asked, and "a" does not re-ask on every press.
		m.wide = msg.items
		if m.wide == nil {
			m.wide = []pickerItem{}
		}
		m.showScope(true)
		if m.expandAtOnce && len(m.wide) == 0 {
			// Opened because this directory had none, and the project has none
			// either. There is nothing here to choose from and nothing to go
			// back to, so let the caller say so in its own words.
			m.exhausted = true
			m.done = true
			return m, tea.Quit
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		return m, m.updateKey(msg)
	}
	return m, nil
}

// toggleScope is the "a" key: the wider list when the scoped one is up, and
// back to the scoped one when it is not. The first widening has to ask the
// server, so it runs as a command and lands as a pickerExpandedMsg.
func (m *pickerModel) toggleScope() tea.Cmd {
	if m.expand == nil || m.expanding {
		return nil
	}
	if m.expanded {
		m.showScope(false)
		return nil
	}
	if m.wide != nil {
		m.showScope(true)
		return nil
	}
	m.expanding = true
	m.expandErr = nil
	expand := m.expand
	return func() tea.Msg {
		items, err := expand()
		return pickerExpandedMsg{items: items, err: err}
	}
}

// showScope swaps which list is on screen. The query survives it: widening is
// an answer to "it is not in here", not a reason to retype what is being
// looked for.
func (m *pickerModel) showScope(expanded bool) {
	m.expanded = expanded
	if expanded {
		m.items = m.wide
	} else {
		m.items = m.scoped
	}
	m.refilter()
}

// pollLive schedules the next read of a live prompt, and is nothing at all for
// a static one.
func (m *pickerModel) pollLive() tea.Cmd {
	if m.live == nil {
		return nil
	}
	return tea.Tick(pickerLiveInterval, func(time.Time) tea.Msg { return pickerLiveMsg{} })
}

func (m *pickerModel) updateKey(key tea.KeyPressMsg) tea.Cmd {
	if m.typing {
		switch key.String() {
		case "ctrl+c":
			m.done = true
			return tea.Quit
		case "esc":
			m.typing = false
			m.query = ""
			m.refilter()
			return nil
		case "enter":
			m.typing = false
			return nil
		case "up", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "ctrl+n":
			if m.cursor < len(m.matches)-1 {
				m.cursor++
			}
		case "backspace", "shift+backspace":
			if q := []rune(m.query); len(q) > 0 {
				m.query = string(q[:len(q)-1])
				m.refilter()
			}
		case "ctrl+u":
			m.query = ""
			m.refilter()
		default:
			if key.Mod&(tea.ModCtrl|tea.ModAlt) == 0 && key.Text != "" {
				m.query += key.Text
				m.refilter()
			}
		}
		m.scrollToCursor()
		return nil
	}
	switch key.String() {
	case "ctrl+c":
		m.done = true
		return tea.Quit
	case "esc":
		m.done = true
		return tea.Quit
	case "/":
		m.typing = true
		m.query = ""
		m.refilter()
	case "a":
		return m.toggleScope()
	case "up", "ctrl+p", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "ctrl+n", "j":
		if m.cursor < len(m.matches)-1 {
			m.cursor++
		}
	case "enter":
		if len(m.matches) == 0 {
			return nil
		}
		m.chosen = m.matches[m.cursor].index
		m.done = true
		return tea.Quit
	}
	m.scrollToCursor()
	return nil
}

// refilter recomputes the visible matches for the current query, keeping the
// item the cursor was on selected when it survives.
func (m *pickerModel) refilter() {
	// By ID, not by index: "a" swaps the whole list underneath, and an index
	// into the old one names a different discobox in the new one.
	selected := ""
	if m.cursor < len(m.matches) {
		selected = m.matches[m.cursor].item.id
	}
	m.matches = fuzzyPickerMatches(m.items, m.query, m.recentID)
	m.cursor = 0
	for i, match := range m.matches {
		if selected != "" && match.item.id == selected {
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
			{item.originText(), pickerDetailWeight, &match.originPos},
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
	// Keep the standalone card in the launcher's visual language. These are the
	// same fixed 256-color entries as internal/tui/theme.go: gold is the one
	// accent, dim text recedes, and the cursor is a whole-row band.
	pickerTitleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	pickerCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	pickerDetailStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	pickerHelpStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	pickerQueryStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	pickerMatchStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	pickerRecentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	pickerCardStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("220")).Padding(1, 2)
	// pickerNoteStyle draws whatever a prompt puts under its first line: the
	// thing the question turns on, set apart from the sentence explaining it.
	pickerNoteStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
)

// scopeLine says which list is on screen whenever that is not the plain
// answer the command asked for: a widened list looks arbitrarily longer
// otherwise, and a load that failed would silently look like it did nothing.
func (m *pickerModel) scopeLine() string {
	switch {
	case m.expanding:
		return "listing every discobox…"
	case m.expandErr != nil:
		return "could not list every discobox: " + m.expandErr.Error()
	case m.expanded:
		return "every discobox in this project"
	case len(m.scoped) == 0:
		// Collapsed back onto the list that was empty to begin with. Without
		// this the card is a bare "no matches" that looks like a broken filter.
		return "nothing was started from this directory"
	default:
		return ""
	}
}

// pickerRowFixed is the row's fixed head — cursor, title column, id column, and
// the gaps between them — and pickerCardChrome the bordered card's own width, a
// border column and two of padding a side. What the terminal has past those is
// all the detail and origin columns get to share.
const (
	pickerRowFixed    = 2 + pickerFieldWidth + 2 + pickerFieldWidth + 2
	pickerCardChrome  = 2 + 4
	pickerFieldWidth  = 24
	pickerColumnGap   = 2
	pickerRecentLabel = " · last used"
)

// columnBudgets divides what the terminal leaves after the fixed columns
// between detail and origin. Detail is the row's answer, so it is served first;
// origin takes what is left and is dropped outright when that is too little to
// read. A card that outgrows the window wraps, and because the picker is an
// inline program rather than a full-screen one, a wrapped card also smears: the
// repaint counts the lines it drew, not the lines the terminal made of them.
func (m *pickerModel) columnBudgets() (detail, origin int) {
	widestDetail, widestOrigin := 0, 0
	for _, match := range m.matches {
		text := match.item.detail
		if match.recent {
			text += pickerRecentLabel
		}
		widestDetail = max(widestDetail, lipgloss.Width(text))
		widestOrigin = max(widestOrigin, lipgloss.Width(match.item.originText()))
	}
	widestOrigin = min(widestOrigin, pickerOriginWidth)
	if m.width <= 0 {
		// Nothing has said how wide the terminal is, which is only ever true
		// before the first frame. Draw it whole rather than guess it narrow.
		return widestDetail, widestOrigin
	}
	available := max(m.width-pickerCardChrome-pickerRowFixed, 0)
	detail = min(widestDetail, available)
	if origin = min(widestOrigin, available-detail-pickerColumnGap); origin < pickerOriginMin {
		origin = 0
	}
	return detail, origin
}

func (m *pickerModel) View() tea.View {
	if m.done {
		// Take the menu back down: the picked resource is the command's subject,
		// not part of its output.
		return tea.NewView("")
	}
	var b strings.Builder
	prompt, note, hasNote := strings.Cut(m.prompt, "\n")
	fmt.Fprintf(&b, "%s\n", pickerTitleStyle.Render(prompt))
	if hasNote {
		fmt.Fprintf(&b, "%s\n", pickerNoteStyle.Render(note))
	}
	if scope := m.scopeLine(); scope != "" {
		if m.width > 0 {
			// The one line here whose length nothing else bounds — a transport
			// error arrives verbatim and is easily wider than the window, and
			// it would size the card's border past everything just budgeted.
			scope, _ = truncatePickerText(scope, m.width-pickerCardChrome)
		}
		fmt.Fprintf(&b, "%s\n", pickerDetailStyle.Render(scope))
	}
	if m.typing || m.query != "" {
		fmt.Fprintf(&b, "%s%s%s\n\n", pickerQueryStyle.Render("/"), m.query, pickerQueryStyle.Render("▏"))
	}
	if len(m.matches) == 0 {
		fmt.Fprintf(&b, "%s\n", pickerDetailStyle.Render("  no matches"))
	}
	detailWidth, originWidth := m.columnBudgets()
	end := min(m.offset+pickerVisible, len(m.matches))
	for i := m.offset; i < end; i++ {
		match := m.matches[i]
		selected := i == m.cursor
		cursor := "  "
		if selected {
			cursor = pickerCursorStyle.Render("❯ ")
		}
		text, positions := match.item.detail, match.detailPos
		label := ""
		if match.recent {
			// Say why this row is on top, so leading with it does not look
			// arbitrary. It rides in the detail column and inside its budget,
			// so it cannot push the card past the window either.
			label = pickerRecentLabel
		}
		if lipgloss.Width(text+label) > detailWidth {
			text, _ = truncatePickerText(text+label, detailWidth)
			positions, label = nil, ""
		}
		detail := renderPickerField(text, positions, 0, pickerDetailStyle, selected)
		if label != "" {
			detail += pickerRecentStyle.Render(label)
		}
		detail = padPickerRow(detail, detailWidth)
		row := fmt.Sprintf("%s%s  %s  %s", cursor,
			renderPickerField(match.item.title, match.titlePos, pickerFieldWidth, lipgloss.NewStyle(), selected),
			renderPickerField(match.item.id, match.idPos, pickerFieldWidth, pickerDetailStyle, selected), detail)
		if origin, shortened := shortenPickerOrigin(match.item, originWidth); origin != "" {
			positions := match.originPos
			if shortened {
				positions = nil
			}
			row += strings.Repeat(" ", pickerColumnGap) +
				renderPickerField(origin, positions, originWidth, pickerDetailStyle, selected)
		}
		row = padPickerRow(row, pickerRowWidth(detailWidth, originWidth))
		if selected {
			row = highlightPickerRow(row)
		}
		fmt.Fprintf(&b, "%s\n", row)
	}
	if hidden := len(m.matches) - end; hidden > 0 {
		fmt.Fprintf(&b, "%s\n", pickerDetailStyle.Render(fmt.Sprintf("  … %d more", hidden)))
	}
	help := "/ search · ↑↓ moves · Enter selects · Esc cancels"
	if m.expand != nil {
		// Say what the key does next, not what is on screen now, so the list
		// and the key never look like they claim different things.
		if m.expanded {
			help += " · a this directory"
		} else {
			help += " · a every discobox"
		}
	}
	if m.typing {
		help = "type to search · Enter finishes · Esc clears"
	}
	fmt.Fprintf(&b, "\n%s\n", pickerHelpStyle.Render(help))
	return tea.NewView(pickerCardStyle.Render(b.String()))
}

// pickerRowWidth is what every row is padded to, so the cursor's band covers
// whole rows and the columns line up under each other.
func pickerRowWidth(detailWidth, originWidth int) int {
	width := pickerRowFixed + detailWidth
	if originWidth > 0 {
		width += pickerColumnGap + originWidth
	}
	return width
}

func padPickerRow(row string, width int) string {
	if pad := width - lipgloss.Width(row); pad > 0 {
		return row + strings.Repeat(" ", pad)
	}
	return row
}

// highlightPickerRow keeps the cursor background alive across the resets from
// each styled field, the same rule the launcher's sandbox table uses.
func highlightPickerRow(row string) string {
	const reset = "\x1b[0m"
	const shortReset = "\x1b[m"
	seq := "\x1b[48;5;237m"
	reassert := strings.NewReplacer(reset, reset+seq, shortReset, shortReset+seq)
	return seq + reassert.Replace(row) + reset
}

// renderPickerField draws one column, highlighting the runes the query matched
// and padding to width so columns line up despite the styling.
//
// A column that is given a width is also cut to it. Padding alone is not
// enough: a sandbox's title is `Sandbox.displayName`, which is whatever window
// title its terminal last set — routinely a path far wider than the column,
// which is why `writeSandboxes` truncates it too. Left to overflow it pushes
// every column after it out of line and the card past the window.
func renderPickerField(text string, positions []int, width int, base lipgloss.Style, selected bool) string {
	if width > 0 && lipgloss.Width(text) > width {
		// The cut keeps the front, so the offsets still standing point at the
		// same runes; the ones past the ellipsis have nothing left to mark.
		var kept int
		text, kept = truncatePickerText(text, width)
		positions = positionsWithin(positions, kept)
	}
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
	if pad := width - lipgloss.Width(text); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}
