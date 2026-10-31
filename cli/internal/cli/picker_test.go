package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

func TestPickOneWithoutChoices(t *testing.T) {
	cmd := &cobra.Command{}
	_, err := pickOne(cmd, "Select a sandbox", nil, pickerOptions{empty: "nothing here", ambiguous: "too many"})
	if err == nil || err.Error() != "nothing here" {
		t.Fatalf("pickOne err = %v, want the empty label", err)
	}
}

func TestPickOneWithSingleChoiceSkipsPrompt(t *testing.T) {
	cmd := &cobra.Command{}
	items := []pickerItem{{id: "sbx_1", title: "only"}}
	id, err := pickOne(cmd, "Select a sandbox", items, pickerOptions{empty: "nothing here", ambiguous: "too many"})
	if err != nil {
		t.Fatalf("pickOne: %v", err)
	}
	if id != "sbx_1" {
		t.Fatalf("pickOne id = %q, want sbx_1", id)
	}
}

// Without a terminal there is nobody to ask, so an ambiguous pick must fail
// with the caller's guidance rather than silently choosing.
func TestPickOneWithoutTerminalIsAmbiguous(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetErr(&bytes.Buffer{})
	items := []pickerItem{{id: "sbx_1"}, {id: "sbx_2"}}
	_, err := pickOne(cmd, "Select a sandbox", items, pickerOptions{empty: "nothing here", ambiguous: "too many"})
	if err == nil || err.Error() != "too many" {
		t.Fatalf("pickOne err = %v, want the ambiguous label", err)
	}
}

// The picker leads with the sandbox the user touched last, so an unfiltered
// list opens on the most likely choice.
func TestSandboxPickerListsMostRecentlyUpdatedFirst(t *testing.T) {
	stale := apimodel.Sandbox{ID: "sbx_stale", UpdatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	stale.Config.Name = "stale"
	stale.DisplayName = "stale"
	fresh := apimodel.Sandbox{ID: "sbx_fresh", UpdatedAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)}
	fresh.Config.Name = "fresh"
	fresh.DisplayName = "fresh"

	m := newPickerModel("Select a sandbox", sandboxPickerItems([]apimodel.Sandbox{stale, fresh}, ""), "")
	if len(m.matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(m.matches))
	}
	if m.matches[0].item.id != "sbx_fresh" || m.matches[1].item.id != "sbx_stale" {
		t.Fatalf("order = %q, %q, want most recently updated first", m.matches[0].item.id, m.matches[1].item.id)
	}
	if m.matches[0].item.title != "fresh" {
		t.Fatalf("item title = %q, want fresh", m.matches[0].item.title)
	}
}

func TestSandboxPickerUsesTheServerDisplayName(t *testing.T) {
	sandbox := apimodel.Sandbox{ID: "sbx_1", DisplayName: "fix the reaper"}
	sandbox.Config.Name = "generated-name"

	items := sandboxPickerItems([]apimodel.Sandbox{sandbox}, "")
	if len(items) != 1 || items[0].title != "fix the reaper" {
		t.Fatalf("picker items = %+v, want the terminal title from displayName", items)
	}
}

func TestSandboxPickerDefensivelyFallsBackToConfiguredNameThenID(t *testing.T) {
	named := apimodel.Sandbox{ID: "sbx_named"}
	named.Config.Name = "configured-name"
	unnamed := apimodel.Sandbox{ID: "sbx_unnamed"}

	items := sandboxPickerItems([]apimodel.Sandbox{named, unnamed}, "")
	if items[0].title != "configured-name" || items[1].title != "sbx_unnamed" {
		t.Fatalf("picker titles = %q, %q, want configured name then ID", items[0].title, items[1].title)
	}
}

// Recency is also the tie-break once a query is scoring items equally.
func TestPickerTiesBreakOnMostRecentlyUpdated(t *testing.T) {
	older := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	m := newPickerModel("Select a sandbox", []pickerItem{
		{id: "sbx_1", title: "api", updatedAt: older},
		{id: "sbx_2", title: "api", updatedAt: newer},
	}, "")
	typePickerKeys(t, m, "/", "a", "p", "i")
	if len(m.matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(m.matches))
	}
	if m.matches[0].item.id != "sbx_2" {
		t.Fatalf("top match = %q, want the more recently updated sbx_2", m.matches[0].item.id)
	}
}

func typePickerKeys(t *testing.T, m *pickerModel, keys ...string) {
	t.Helper()
	for _, key := range keys {
		var msg tea.KeyPressMsg
		switch key {
		case "up", "down", "backspace", "esc", "enter":
			codes := map[string]rune{"up": tea.KeyUp, "down": tea.KeyDown, "backspace": tea.KeyBackspace, "esc": tea.KeyEscape, "enter": tea.KeyEnter}
			msg = tea.KeyPressMsg{Code: codes[key]}
		default:
			msg = tea.KeyPressMsg{Code: []rune(key)[0], Text: key}
		}
		m.Update(msg)
	}
}

func TestPickerTypingFiltersAndRanks(t *testing.T) {
	m := newPickerModel("Select a sandbox", []pickerItem{
		{id: "sbx_aaa", title: "docs-site", detail: "running · now"},
		{id: "sbx_bbb", title: "api-server", detail: "stopped · now"},
		{id: "sbx_ccc", title: "apiary", detail: "running · now"},
	}, "")
	typePickerKeys(t, m, "/", "a", "p", "i")
	if len(m.matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(m.matches))
	}
	// "apiary" matches contiguously from the start, so it outranks "api-server"
	// only if scoring rewards the run; both must beat the non-matching item.
	ids := []string{m.matches[0].item.id, m.matches[1].item.id}
	if ids[0] != "sbx_ccc" && ids[0] != "sbx_bbb" {
		t.Fatalf("top match = %q, want an api* sandbox", ids[0])
	}
	for _, id := range ids {
		if id == "sbx_aaa" {
			t.Fatalf("docs-site matched query %q", m.query)
		}
	}
}

func TestPickerBackspaceAndEscapeRestoreTheFullList(t *testing.T) {
	m := newPickerModel("Select a sandbox", []pickerItem{
		{id: "sbx_aaa", title: "docs"},
		{id: "sbx_bbb", title: "api"},
	}, "")
	typePickerKeys(t, m, "/", "a", "p", "i")
	if len(m.matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(m.matches))
	}
	typePickerKeys(t, m, "backspace")
	if m.query != "ap" {
		t.Fatalf("query = %q, want ap", m.query)
	}
	typePickerKeys(t, m, "esc")
	if m.query != "" || len(m.matches) != 2 {
		t.Fatalf("after esc: query = %q, matches = %d, want empty query and 2 matches", m.query, len(m.matches))
	}
	if m.done {
		t.Fatal("esc with a query set canceled the picker instead of clearing the query")
	}
	typePickerKeys(t, m, "esc")
	if !m.done || m.chosen != -1 {
		t.Fatalf("second esc: done = %v, chosen = %d, want cancel", m.done, m.chosen)
	}
}

func TestPickerEnterChoosesTheHighlightedMatch(t *testing.T) {
	m := newPickerModel("Select a sandbox", []pickerItem{
		{id: "sbx_aaa", title: "docs"},
		{id: "sbx_bbb", title: "api"},
		{id: "sbx_ccc", title: "apex"},
	}, "")
	typePickerKeys(t, m, "/", "a", "p", "down", "enter", "enter")
	if !m.done {
		t.Fatal("enter did not finish the picker")
	}
	want := m.matches[1].item.id
	if got := m.items[m.chosen].id; got != want {
		t.Fatalf("chosen = %q, want %q", got, want)
	}
	if m.items[m.chosen].id == "sbx_aaa" {
		t.Fatal("enter chose a filtered-out item")
	}
}

func TestPickerEnterWithNoMatchesDoesNothing(t *testing.T) {
	m := newPickerModel("Select a sandbox", []pickerItem{{id: "sbx_aaa", title: "docs"}, {id: "sbx_bbb", title: "api"}}, "")
	typePickerKeys(t, m, "/", "z", "z", "z", "enter", "enter")
	if m.done || m.chosen != -1 {
		t.Fatalf("done = %v, chosen = %d, want the picker to stay open", m.done, m.chosen)
	}
}

// The pick from last time leads the unfiltered list, ahead of a more recently
// updated sandbox, because it is the better guess at what the user wants again.
func TestPickerLeadsWithTheLastSelection(t *testing.T) {
	older := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	items := []pickerItem{
		{id: "sbx_last", title: "chosen-before", updatedAt: older},
		{id: "sbx_fresh", title: "touched-since", updatedAt: newer},
	}
	m := newPickerModel("Select a sandbox", items, "sbx_last")
	if m.matches[0].item.id != "sbx_last" || !m.matches[0].recent {
		t.Fatalf("top match = %+v, want the remembered sbx_last marked recent", m.matches[0])
	}
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want the remembered pick preselected", m.cursor)
	}

	// Typing hands ranking back to the query: the remembered pick gets no
	// standing edge once the user says what they are looking for.
	typePickerKeys(t, m, "/", "t")
	if m.matches[0].item.id != "sbx_fresh" {
		t.Fatalf("top match after typing = %q, want the better query match sbx_fresh", m.matches[0].item.id)
	}
	for _, match := range m.matches {
		if match.recent {
			t.Fatalf("match %q still marked recent while filtering", match.item.id)
		}
	}
}

// A remembered sandbox that is gone (deleted, or from another project) must not
// disturb the list.
func TestPickerIgnoresAnUnknownLastSelection(t *testing.T) {
	items := []pickerItem{
		{id: "sbx_1", title: "one", updatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{id: "sbx_2", title: "two", updatedAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)},
	}
	m := newPickerModel("Select a sandbox", items, "sbx_gone")
	if m.matches[0].item.id != "sbx_2" {
		t.Fatalf("top match = %q, want the most recently updated sbx_2", m.matches[0].item.id)
	}
}

// A live prompt is polled until it says the number in it is final, so a
// question about a directory still being measured comes up straight away and
// fills its size in as the walk finds it.
func TestPickerLivePromptFollowsItsSubjectUntilItIsFinal(t *testing.T) {
	counted := 0
	live := func() (string, bool) {
		counted++
		if counted < 2 {
			return "calculating… 1.0 MiB so far", false
		}
		return "2.0 MiB", true
	}
	m := newPickerModel("calculating… 0 B so far", []pickerItem{{id: "no"}, {id: "yes"}}, "")
	m.live = live

	if cmd := m.Init(); cmd == nil {
		t.Fatal("a live prompt should schedule its first read")
	}
	if _, cmd := m.Update(pickerLiveMsg{}); cmd == nil {
		t.Fatal("a prompt that is still counting should schedule another read")
	}
	if m.prompt != "calculating… 1.0 MiB so far" {
		t.Fatalf("prompt = %q, want the running count", m.prompt)
	}
	if _, cmd := m.Update(pickerLiveMsg{}); cmd != nil {
		t.Fatal("a final prompt should stop polling")
	}
	if m.prompt != "2.0 MiB" {
		t.Fatalf("prompt = %q, want the final count", m.prompt)
	}
}

// What a prompt puts under its first line is drawn apart from it, so a question
// whose answer turns on a number shows the number rather than burying it in the
// sentence.
func TestPickerDrawsAPromptsNoteUnderIt(t *testing.T) {
	m := newPickerModel("this directory is not a Git repository:\n4.8 GiB in 141201 files", []pickerItem{{id: "no"}, {id: "yes"}}, "")
	lines := strings.Split(m.View().Content, "\n")
	question := -1
	for i, line := range lines {
		if strings.Contains(line, "not a Git repository") {
			question = i
			break
		}
	}
	if question < 0 {
		t.Fatalf("view has no question:\n%s", m.View().Content)
	}
	if question+1 >= len(lines) {
		t.Fatalf("question has no following note:\n%s", m.View().Content)
	}
	if !strings.Contains(lines[question+1], "4.8 GiB in 141201 files") {
		t.Fatalf("line after question = %q, want the note on a line of its own", lines[question+1])
	}
}

func TestPickerUsesTheTUIDialogAndTableLanguage(t *testing.T) {
	m := newPickerModel("Select a discobox", []pickerItem{{id: "sbx_1", title: "api", detail: "running"}, {id: "sbx_2", title: "docs", detail: "stopped"}}, "")
	view := m.View().Content
	for _, want := range []string{"╭", "╯", "❯", "/ search", "Enter selects", "Esc cancels"} {
		if !strings.Contains(view, want) {
			t.Fatalf("picker view has no %q:\n%s", want, view)
		}
	}
	if strings.Index(view, "api") > strings.Index(view, "sbx_1") {
		t.Fatalf("picker row puts the opaque ID before the name:\n%s", view)
	}
	if !strings.Contains(view, "\x1b[m\x1b[48;5;237m") {
		t.Fatalf("picker cursor background was not reasserted after styled fields:\n%s", view)
	}
	if strings.Contains(view, "/▏") {
		t.Fatalf("picker showed a search field before / was pressed:\n%s", view)
	}
	typePickerKeys(t, m, "/", "a")
	if view := m.View().Content; !strings.Contains(view, "type to search") || m.query != "a" || !m.typing {
		t.Fatalf("/ did not open the search field:\n%s", view)
	}
}

// A static prompt polls nothing at all.
func TestPickerWithoutALivePromptSchedulesNothing(t *testing.T) {
	m := newPickerModel("Select a discobox", []pickerItem{{id: "sbx_1"}, {id: "sbx_2"}}, "")
	if cmd := m.Init(); cmd != nil {
		t.Fatal("a static prompt should schedule nothing")
	}
}

// The question about a directory in no repository leads with what it would
// cost, and says so as a running count until the walk behind it is done.
func TestDirectoryCopyPromptSaysWhatItWouldCopy(t *testing.T) {
	counting := strings.Split(directoryCopyPrompt("/home/ada", sandboxcreate.DirectoryTotal{Bytes: 5 << 20, Files: 3}), "\n")
	if len(counting) != 2 {
		t.Fatalf("prompt = %q, want the size on a line of its own", counting)
	}
	if !strings.Contains(counting[0], "/home/ada") || !strings.Contains(counting[0], "not a Git repository") {
		t.Fatalf("prompt = %q, want the directory named", counting[0])
	}
	if counting[1] != "5.0 MiB in 3 files, still counting…" {
		t.Fatalf("size line = %q, want a count that is still climbing", counting[1])
	}
	done := directoryCopyPrompt("/home/ada", sandboxcreate.DirectoryTotal{Bytes: 1 << 20, Files: 1, Done: true})
	if got := strings.Split(done, "\n")[1]; got != "1.0 MiB in 1 file" {
		t.Fatalf("size line = %q, want the final count stated as one", got)
	}
	// Before the walk has reached anything, a zero would be a number that is
	// about to be wrong.
	started := directoryCopyPrompt("/home/ada", sandboxcreate.DirectoryTotal{})
	if got := strings.Split(started, "\n")[1]; got != "calculating…" {
		t.Fatalf("size line = %q, want the walk to say it has nothing yet", got)
	}
}

// The rows say where each discobox came from — its source — since that is all
// that tells two identically named discoboxes from different places apart.
func TestSandboxPickerItemsSayWhereARowCameFrom(t *testing.T) {
	cutFrom := func(sb *apimodel.Sandbox, dir string) {
		sb.Config.SetSource(apiclientgen.NewOptGitSource(apimodel.GitSource{LocalDirectory: apiclientgen.NewOptString(dir)}))
	}
	here := apimodel.Sandbox{ID: "sbx_here", DisplayName: "here"}
	here.Origin = apiclientgen.NewOptOrigin(apimodel.Origin{HostId: "host_local"})
	cutFrom(&here, "/home/ada/here")
	there := apimodel.Sandbox{ID: "sbx_there", DisplayName: "there"}
	there.Origin = apiclientgen.NewOptOrigin(apimodel.Origin{
		HostId:   "host_other",
		Hostname: apiclientgen.NewOptString("laptop"),
	})
	cutFrom(&there, "/home/ada/there")
	nowhere := apimodel.Sandbox{ID: "sbx_nowhere", DisplayName: "nowhere"}
	empty := apimodel.Sandbox{ID: "sbx_empty", DisplayName: "empty"}
	empty.Origin = apiclientgen.NewOptOrigin(apimodel.Origin{HostId: "host_local"})

	wide := sandboxPickerItems([]apimodel.Sandbox{here, there, nowhere, empty}, "host_local")
	if wide[0].originText() != "/home/ada/here" {
		t.Fatalf("local row origin = %q, want the directory alone", wide[0].originText())
	}
	if wide[1].originText() != "laptop:/home/ada/there" || wide[1].originHost != "laptop" {
		t.Fatalf("remote row origin = %q, want the machine named and kept apart", wide[1].originText())
	}
	if wide[2].originText() != "" {
		t.Fatalf("a discobox with no origin got one: %q", wide[2].originText())
	}
	if wide[3].originText() != "no source" {
		t.Fatalf("sourceless row origin = %q, want it to say it has no source", wide[3].originText())
	}
	// The origin is its own column, so it never lands in the detail text that
	// the runtime state and the git word share.
	for _, item := range wide {
		if strings.Contains(item.detail, "/home/ada") {
			t.Fatalf("%s detail swallowed the origin: %q", item.id, item.detail)
		}
	}
}

// A picker row carries `discobox ls`'s CHANGES word, so the choice between two
// discoboxes can be made on which one holds work rather than on the name alone.
func TestSandboxPickerRowsCarryTheGitState(t *testing.T) {
	running := func(id, agentSources string) apimodel.Sandbox {
		sb := gitStateSandbox(t, spawnSHA, agentSources)
		sb.ID = id
		sb.DisplayName = id
		sb.UpdatedAt = time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
		sb.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState(apiclientgen.SandboxRuntimeDisplayStateRunning)
		return sb
	}
	dirty := running("sbx_dirty", sprintfSources("false", headSHA))
	ready := running("sbx_ready", sprintfSources("true", headSHA))
	clean := running("sbx_clean", sprintfSources("true", spawnSHA))
	unreported := running("sbx_quiet", "")

	items := sandboxPickerItems([]apimodel.Sandbox{dirty, ready, clean, unreported}, "")
	for i, want := range []string{"dirty", "ready", "clean"} {
		if !strings.Contains(items[i].detail, " · "+want+" · ") {
			t.Fatalf("%s row detail = %q, want the git state after the runtime state", items[i].id, items[i].detail)
		}
	}
	// Nothing reported is left out rather than drawn as a dash: a blank column
	// reads as a column, a dash mid-sentence reads as a value.
	if strings.Contains(items[3].detail, "-") || strings.Count(items[3].detail, "·") != 1 {
		t.Fatalf("unreported row detail = %q, want only the runtime state and the time", items[3].detail)
	}
}

// A card wider than the terminal does not merely wrap for an inline program,
// it smears, so the origin column lives inside what the window leaves and is
// dropped outright when that is too little to read.
func TestPickerOriginColumnFitsTheTerminal(t *testing.T) {
	items := []pickerItem{
		// A real displayName is whatever window title the discobox's terminal
		// last set — usually a path, and wider than the column it goes in.
		{id: "sbx_1", title: "ada@laptop: ~/src/discobox/cli/internal/cli", detail: "running · dirty · 3 hours ago", origin: "/home/ada/src/discobox"},
		{id: "sbx_2", title: "docs", detail: "stopped · 5 days ago", origin: "/home/ada/src/other"},
	}
	// A wide rune is one rune and two cells, so a picker that decided in runes
	// and padded in cells would come apart here and nowhere in ASCII.
	wide := append(append([]pickerItem{}, items...), pickerItem{
		id: "sbx_3", title: strings.Repeat("設定", 15), detail: "running · 2 days ago", origin: "/home/ada/src/" + strings.Repeat("設定", 12),
	})
	for _, tc := range []struct {
		width      int
		wantOrigin bool
		items      []pickerItem
	}{
		{width: 80, wantOrigin: false, items: items},
		{width: 100, wantOrigin: false, items: items},
		{width: 140, wantOrigin: true, items: items},
		{width: 80, wantOrigin: false, items: wide},
		{width: 140, wantOrigin: true, items: wide},
	} {
		items := tc.items
		m := newPickerModel("Select a discobox", items, "")
		m.Update(tea.WindowSizeMsg{Width: tc.width, Height: 40})
		view := m.View().Content
		for _, line := range strings.Split(view, "\n") {
			if got := lipgloss.Width(line); got > tc.width {
				t.Fatalf("at %d columns a line came out %d wide: %q", tc.width, got, line)
			}
		}
		// The title carries "~/src/…" of its own, so match the absolute form
		// only an origin has.
		if got := strings.Contains(view, "/home/ada/src/"); got != tc.wantOrigin {
			t.Fatalf("at %d columns origin drawn = %v, want %v:\n%s", tc.width, got, tc.wantOrigin, view)
		}
		// Every row is the same width, or the columns after the long title are
		// out of line with the ones above them.
		widths := map[int]bool{}
		for _, line := range strings.Split(strings.TrimRight(view, "\n"), "\n") {
			widths[lipgloss.Width(line)] = true
		}
		if len(widths) != 1 {
			t.Fatalf("at %d columns the card is ragged (%v):\n%s", tc.width, widths, view)
		}
	}
}

// The machine name is the whole reason a remote row can be told from a local
// one, so shortening takes the room out of the path and never out of it.
func TestShortenPickerOriginKeepsTheMachineName(t *testing.T) {
	item := pickerItem{originHost: "laptop", origin: "/home/ada/src/discobox/worktrees/feature-branch-two"}
	got, shortened := shortenPickerOrigin(item, 40)
	if !shortened {
		t.Fatalf("shortenPickerOrigin(%d) = %q, want it shortened", 40, got)
	}
	if len([]rune(got)) > 40 {
		t.Fatalf("shortened = %q, %d runes, want at most 40", got, len([]rune(got)))
	}
	if !strings.HasPrefix(got, "laptop:") {
		t.Fatalf("shortened = %q, want the machine name kept whole", got)
	}
	if !strings.HasSuffix(got, "feature-branch-two") {
		t.Fatalf("shortened = %q, want the end of the path kept", got)
	}

	// A path that fits is left alone, and says so, because only a shortened
	// origin has to give up its match highlighting.
	local := pickerItem{origin: "/home/ada"}
	if got, shortened := shortenPickerOrigin(local, 40); got != "/home/ada" || shortened {
		t.Fatalf("shortenPickerOrigin = %q, %v, want the path untouched", got, shortened)
	}

	// A Windows path carries a colon of its own; the host is a separate field
	// precisely so nothing has to guess where one ends and the other begins.
	win := pickerItem{origin: `C:\Users\ada\src\discobox\a\very\long\project\path\here`}
	if got, _ := shortenPickerOrigin(win, 30); !strings.HasPrefix(got, "…") {
		t.Fatalf("shortened windows path = %q, want its front elided, not split on the drive colon", got)
	}
}

// One candidate is taken without asking: stopping to ask would cost every such
// run a keystroke to answer a question with one answer.
func TestPickOneTakesASingleCandidateWithoutAsking(t *testing.T) {
	cmd := &cobra.Command{}
	id, err := pickOne(cmd, "Select a discobox", []pickerItem{{id: "sbx_only"}}, pickerOptions{
		empty:     "nothing here",
		ambiguous: "too many",
	})
	if err != nil || id != "sbx_only" {
		t.Fatalf("pickOne = %q, %v, want the one candidate", id, err)
	}
}

// Nothing to choose from fails with the caller's wording rather than opening
// an empty card.
func TestPickOneWithNoCandidatesFails(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetErr(&bytes.Buffer{})
	_, err := pickOne(cmd, "Select a discobox", nil, pickerOptions{
		empty:     "nothing here",
		ambiguous: "too many",
	})
	if err == nil || err.Error() != "nothing here" {
		t.Fatalf("pickOne err = %v, want the empty label", err)
	}
}
