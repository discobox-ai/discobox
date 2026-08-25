package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

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
	fresh := apimodel.Sandbox{ID: "sbx_fresh", UpdatedAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)}
	fresh.Config.Name = "fresh"

	m := newPickerModel("Select a sandbox", sandboxPickerItems([]apimodel.Sandbox{stale, fresh}), "")
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

// Recency is also the tie-break once a query is scoring items equally.
func TestPickerTiesBreakOnMostRecentlyUpdated(t *testing.T) {
	older := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	m := newPickerModel("Select a sandbox", []pickerItem{
		{id: "sbx_1", title: "api", updatedAt: older},
		{id: "sbx_2", title: "api", updatedAt: newer},
	}, "")
	typePickerKeys(t, m, "a", "p", "i")
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
	typePickerKeys(t, m, "a", "p", "i")
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
	typePickerKeys(t, m, "a", "p", "i")
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
	typePickerKeys(t, m, "a", "p", "down", "enter")
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
	typePickerKeys(t, m, "z", "z", "z", "enter")
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
	typePickerKeys(t, m, "t")
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
	counting := directoryCopyPrompt("/home/ada", sandboxcreate.DirectoryTotal{Bytes: 5 << 20, Files: 3})
	if !strings.Contains(counting, "/home/ada") || !strings.Contains(counting, "not a Git repository") {
		t.Fatalf("prompt = %q, want the directory named", counting)
	}
	if !strings.Contains(counting, "calculating… 5.0 MiB in 3 files so far") {
		t.Fatalf("prompt = %q, want a count that is still climbing", counting)
	}
	done := directoryCopyPrompt("/home/ada", sandboxcreate.DirectoryTotal{Bytes: 1 << 20, Files: 1, Done: true})
	if strings.Contains(done, "calculating") || !strings.Contains(done, "1.0 MiB in 1 file") {
		t.Fatalf("prompt = %q, want the final count stated as one", done)
	}
}
