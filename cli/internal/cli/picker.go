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
	"github.com/discobox-ai/discobox/pkg/endpoint"
)

// errPickCanceled reports that the user dismissed a picker without choosing.
var errPickCanceled = errors.New("canceled")

// selectSandbox resolves the discobox a command acts on, and the server it is
// on.
//
// A discobox's address — discobox://<server>/<discobox> — names both, and a
// server first reached through one is registered (ADR 0116 §6). A bare ID or
// prefix is looked for on the primary and then on the registered servers, so
// an ID copied from `discobox ls` works whichever server listed it. With
// neither, the candidates are what `discobox ls` shows, across every server,
// and the user picks one when there is more than one.
//
// app is this invocation aimed at the discobox's server. The caller carries on
// with it in place of the App it called this on: everything after — the
// terminal, the git transport, the next request — has to reach the same
// server.
func (a *App) selectSandbox(cmd *cobra.Command, sandboxArg string) (app *App, projectID string, sandboxID string, client *apiclientgen.Client, err error) {
	if address, ok, err := endpoint.ParseSandboxAddress(sandboxArg); ok {
		if err != nil {
			return nil, "", "", nil, err
		}
		return a.selectAddressedSandbox(cmd, address)
	}
	set, err := a.servers()
	if err != nil {
		return nil, "", "", nil, err
	}
	if strings.TrimSpace(sandboxArg) != "" {
		if len(set) > 1 {
			target, projectID, sandboxID, client, err := a.findOnEveryServer(cmd.Context(), set, sandboxArg)
			if err != nil {
				return nil, "", "", nil, err
			}
			return target.app, projectID, sandboxID, client, nil
		}
		projectID, err = a.projectIDValue()
		if err != nil {
			return nil, "", "", nil, err
		}
		client, err = a.apiClient()
		if err != nil {
			return nil, "", "", nil, err
		}
		sandboxID, err = a.resolveSandboxID(cmd.Context(), client, projectID, sandboxArg)
		return a, projectID, sandboxID, client, err
	}

	candidates, unreachable, err := a.sandboxCandidates(cmd.Context())
	if err != nil {
		return nil, "", "", nil, err
	}
	target, sandboxID, err := a.pickServerSandbox(cmd, set, candidates, unreachable,
		"there are no discoboxes; start one with `discobox run`",
		"there is more than one discobox; pass --discobox-id")
	if err != nil {
		return nil, "", "", nil, err
	}
	projectID, client, err = target.projectClient(cmd.Context())
	if err != nil {
		return nil, "", "", nil, err
	}
	return target.app, projectID, sandboxID, client, nil
}

// pickServerSandbox is the picker over candidates already listed across every
// server (sandboxCandidates), so a caller that listed them to match a name
// against does not list them a second time to offer them. It answers with the
// server the picked discobox is on and the discobox's ID.
//
// The caller says what an empty or ambiguous list means in its own words,
// since which flag or argument would have named one is the caller's; the rest
// of the picker's options are the same for every caller.
func (a *App) pickServerSandbox(cmd *cobra.Command, set []*server, candidates []serverSandbox, unreachable []unansweredServer, empty, ambiguous string) (*server, string, error) {
	note := printedNotes(cmd.ErrOrStderr())
	for _, silent := range unreachable {
		note("%s did not answer, so its discoboxes are not offered: %v", silent.server.name, silent.err)
	}
	several := len(set) > 1
	projectID, err := a.projectIDValue()
	if err != nil {
		return nil, "", err
	}
	localHostID, err := hostid.Get()
	if err != nil {
		return nil, "", err
	}
	picked, err := pickOne(cmd, "Select a discobox", serverSandboxPickerItems(candidates, localHostID, several), pickerOptions{
		empty:     empty,
		ambiguous: ambiguous,
		// The remembered pick is per project, because the candidate list is.
		recentKey: "sandbox:" + projectID,
	})
	if err != nil {
		return nil, "", err
	}
	// A row from a registered server is keyed by the server's name as well as
	// the discobox's ID (serverSandboxPickerItems), which is what says where to
	// go from here.
	target, sandboxID := set[0], picked
	if name, id, ok := strings.Cut(picked, "/"); ok {
		if target, ok = serverNamed(set[1:], name); !ok {
			return nil, "", fmt.Errorf("picked a discobox on %s, which is not a server this command knows", name)
		}
		sandboxID = id
	}
	return target, sandboxID, nil
}

// matchServerSandboxArg is matchSandboxArg over candidates listed across every
// server: a name or a short ID matched among them under the same rules, and the
// server the matching row was listed from. IDs are unique across servers, so a
// match names one row.
//
// A full generated ID is not looked for here. matchSandboxArg trusts one by its
// shape alone, and which server holds it is a question for findOnEveryServer,
// which asks every server rather than the candidates, so an archived one is
// found too; callers send it there first.
func matchServerSandboxArg(arg string, candidates []serverSandbox, match nameMatch) (*server, string, bool, error) {
	sandboxes := make([]apimodel.Sandbox, 0, len(candidates))
	for _, row := range candidates {
		sandboxes = append(sandboxes, row.sandbox)
	}
	id, ok, err := matchSandboxArg(arg, sandboxes, match)
	if err != nil || !ok {
		return nil, "", false, err
	}
	for _, row := range candidates {
		if row.sandbox.ID == id {
			return row.server, id, true, nil
		}
	}
	return nil, "", false, nil
}

// sandboxCandidates is the picker's list across every server: what `discobox
// ls` shows, less the archived discoboxes, which have no runtime to act on.
func (a *App) sandboxCandidates(ctx context.Context) ([]serverSandbox, []unansweredServer, error) {
	listed, unreachable, err := a.listEveryServer(ctx, sandboxFilter{})
	if err != nil {
		return nil, nil, err
	}
	candidates := make([]serverSandbox, 0, len(listed))
	for _, row := range listed {
		if row.sandbox.Runtime.DesiredState == apiclientgen.SandboxRuntimeDesiredStatePresent {
			candidates = append(candidates, row)
		}
	}
	return candidates, unreachable, nil
}

// serverSandboxPickerItems is sandboxPickerItems for a list that may span
// servers. With several, each row's detail says which server it is on, and a
// row from a registered server is keyed <server>/<id>: IDs are the picker's
// keys, and the pick has to say where to go as well as what to open.
func serverSandboxPickerItems(rows []serverSandbox, localHostID string, several bool) []pickerItem {
	items := make([]pickerItem, 0, len(rows))
	for _, row := range rows {
		item := sandboxPickerItem(row.sandbox, localHostID)
		if several {
			item.detail += " · on " + row.server.name
			if !row.server.primary {
				item.id = row.server.name + "/" + item.id
			}
		}
		items = append(items, item)
	}
	return items
}

// listProjectSandboxCandidates is the candidate list of the commands that take
// a discobox by name or offer a pick, on one server: what `discobox ls` lists,
// less the archived discoboxes. Those remain in `discobox ls` and explicit
// resource completion so they can be inspected and unarchived, but they have
// no runtime and cannot be selected for an operation.
func (a *App) listProjectSandboxCandidates(ctx context.Context, client *apiclientgen.Client, projectID string) ([]apimodel.Sandbox, error) {
	sandboxes, err := a.listProjectSandboxes(ctx, client, projectID, sandboxFilter{})
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

// sandboxPickerItems renders the picker rows for sandboxes. A row's detail is
// `discobox ls`'s answer in the same words: the runtime state, then whether the
// working tree holds work — dirty, ready, applied, clean — then when it was
// last touched. The git word is dropped rather than drawn as "-" when no agent
// has reported: an empty column reads as a column in a table, but as noise in a
// sentence.
//
// localHostID is this machine's host ID, so each row can say where its
// discobox came from and name the machine when that is not this one: the list
// crosses directories and machines, and that is the only thing telling two
// identically named discoboxes apart.
func sandboxPickerItems(sandboxes []apimodel.Sandbox, localHostID string) []pickerItem {
	items := make([]pickerItem, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		items = append(items, sandboxPickerItem(sandbox, localHostID))
	}
	return items
}

// sandboxPickerItem is one sandboxPickerItems row.
func sandboxPickerItem(sandbox apimodel.Sandbox, localHostID string) pickerItem {
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
	item.origin, item.originHost = sandboxPickerOrigin(sandbox, localHostID)
	return item
}

// sandboxPickerOrigin says where a discobox came from: its source — the
// directory or repository URL it was cut from, or that it had none — and the
// machine when that machine is not this one. It is the picker's reading of the
// SOURCE column `discobox ls` carries for the same reason. The two are kept
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
	// The hostname is the creator's claim, as the source is (sandboxSource).
	host = terminalSafe(strings.TrimSpace(origin.Hostname.Or("")))
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
func pickOne(cmd *cobra.Command, prompt string, items []pickerItem, opts pickerOptions) (string, error) {
	askable := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.ErrOrStderr())
	switch {
	case len(items) == 1:
		return items[0].id, nil
	case len(items) == 0:
		return "", errors.New(opts.empty)
	case len(items) > 1 && !askable:
		return "", errors.New(opts.ambiguous)
	}
	model := newPickerModel(prompt, items, lastSelection(opts.recentKey))
	model.live = opts.live
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
	// width is the terminal's, from the runtime. Zero until it has said, which
	// is only ever before the first frame.
	width int
}

// pickerLiveInterval is how often a live prompt is re-read. It is a number
// climbing on screen, so it wants to be seen moving and not much more.
const pickerLiveInterval = 150 * time.Millisecond

// pickerLiveMsg is the poll of a live prompt coming due.
type pickerLiveMsg struct{}

func newPickerModel(prompt string, items []pickerItem, recentID string) *pickerModel {
	m := &pickerModel{prompt: prompt, items: items, recentID: recentID, chosen: -1}
	m.refilter()
	return m
}

func (m *pickerModel) Init() tea.Cmd {
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
	case tea.WindowSizeMsg:
		m.width = msg.Width
		return m, nil
	case tea.KeyPressMsg:
		return m, m.updateKey(msg)
	}
	return m, nil
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
