package console

import (
	"strings"
	"testing"
)

// paletteIDs is the rows as one string: a box by its ID, a command by its
// ID behind a colon, and a row that cannot be chosen marked with a bang.
func paletteIDs(rows []PaletteRow) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		var id string
		if r.Box != nil {
			id = r.Box.ID
		} else {
			id = ":" + r.Command.ID
		}
		if r.Why != "" {
			id = "!" + id
		}
		out = append(out, id)
	}
	return strings.Join(out, " ")
}

func paletteList() *List {
	stopped := here("sbx_q0000003", "quiet")
	stopped.State = StateStopped
	archived := here("sbx_archived1", "old")
	archived.State = StateArchived
	return listOf(here("sbx_alpha111", "alpha"), here("sbx_beta2222", "beta"), stopped, archived)
}

// With no chip a query finds boxes and commands; an exact command name puts
// the command first, an ID-shaped query the boxes.
func TestPaletteFindsBoxesAndCommands(t *testing.T) {
	r, l := NewRegistry(servers{}), paletteList()
	got := r.Palette(l, Context{}, PaletteQuery{Text: "stop"}).Rows
	if len(got) == 0 || got[0].Command == nil || got[0].Command.ID != "stop" {
		t.Fatalf("an exact command is not first: %s", paletteIDs(got))
	}
	got = r.Palette(l, Context{}, PaletteQuery{Text: "sbx_be"}).Rows
	if len(got) == 0 || got[0].Box == nil || got[0].Box.ID != "sbx_beta2222" {
		t.Fatalf("an ID-shaped query does not put the box first: %s", paletteIDs(got))
	}
	for _, row := range r.Palette(l, Context{}, PaletteQuery{}).Rows {
		if row.Box != nil && row.Box.State == StateArchived {
			t.Fatal("archived boxes are searched without being asked for")
		}
	}
}

// An empty query puts the box the window is on first, then the boxes
// visited, latest first, then the commands by use, then the rest of the
// boxes in the list's order.
func TestPaletteRanksAnEmptyQuery(t *testing.T) {
	r, l := NewRegistry(servers{}), paletteList()
	r.Use("refresh")
	r.Use("refresh")
	r.Use("quit")
	q := PaletteQuery{
		Current: BoxRef{Server: "alpha", ID: "sbx_q0000003"},
		Recent:  map[BoxRef]int{{Server: "alpha", ID: "sbx_alpha111"}: 1, {Server: "alpha", ID: "sbx_beta2222"}: 2},
	}
	got := paletteIDs(r.Palette(l, Context{}, q).Rows)
	if !strings.HasPrefix(got, "sbx_q0000003 sbx_beta2222 sbx_alpha111 :refresh :quit ") {
		t.Fatalf("rows = %s", got)
	}
}

// A command chip evaluates the boxes for it: those it can act on, or, when
// there are none, every match dimmed with why. A chip that acts only on
// archived boxes brings them into scope.
func TestPaletteCommandChipEvaluatesBoxes(t *testing.T) {
	r, l := NewRegistry(servers{}), paletteList()
	res := r.Palette(l, Context{}, PaletteQuery{Command: "start"})
	if got := paletteIDs(res.Rows); got != "sbx_q0000003" {
		t.Fatalf("start offers %s", got)
	}
	if got := paletteIDs(r.Palette(l, Context{}, PaletteQuery{Command: "start", Text: "beta"}).Rows); got != "!sbx_beta2222" {
		t.Fatalf("start on beta offers %s, want it dimmed with why", got)
	}
	res = r.Palette(l, Context{}, PaletteQuery{Command: "purge"})
	if !res.Archived || paletteIDs(res.Rows) != "sbx_archived1" {
		t.Fatalf("purge offers %s, archived %v", paletteIDs(res.Rows), res.Archived)
	}
}

// A box chip offers the commands that take a box, evaluated for it; a box no
// longer listed leaves every one dimmed with why.
func TestPaletteBoxChipEvaluatesCommands(t *testing.T) {
	r, l := NewRegistry(servers{}), paletteList()
	box := BoxRef{Server: "alpha", ID: "sbx_q0000003"}
	for _, row := range r.Palette(l, Context{}, PaletteQuery{Box: &box}).Rows {
		if row.Box != nil || row.Command.Target != TargetBox {
			t.Fatalf("a box chip offers %s", paletteIDs([]PaletteRow{row}))
		}
		if row.Command.ID == "stop" {
			t.Fatal("stop is offered for a stopped box")
		}
	}
	gone := BoxRef{Server: "alpha", ID: "sbx_gone"}
	for _, row := range r.Palette(l, Context{}, PaletteQuery{Box: &gone}).Rows {
		if row.Why != "the box is no longer listed" {
			t.Fatalf("a row for a box gone: %+v", row)
		}
	}
}

// Typing a box command's single-word alias and a space commits it as the
// chip; a multiword name, a window command, an ID or no space does not.
func TestPaletteCommitsAnExactAliasOnASpace(t *testing.T) {
	for text, want := range map[string]string{
		"stop hcp": "stop", "RM x": "archive", "stop": "", "sto x": "", "quit now": "",
		"sbx_stop x": "", "review config": "", "": "",
	} {
		id, rest, ok := PaletteCommit(text)
		if id != want || ok != (want != "") {
			t.Errorf("%q commits %q (%v), want %q", text, id, ok, want)
		}
		if ok && rest != text[strings.Index(text, " ")+1:] {
			t.Errorf("%q leaves %q", text, rest)
		}
	}
}

// Every command is found in the palette by its name, wherever the window is:
// one that cannot run there is still listed, dimmed with why, when nothing
// that can matches.
func TestPaletteFindsEveryCommandByName(t *testing.T) {
	r, l := NewRegistry(servers{}), paletteList()
	for name, ctx := range map[string]Context{"the list": {ListHasKeys: true, Tree: true, Themed: true}, "a pane": {HasWorkspace: true, WorkspaceShown: true, PaneFocused: true, Tree: true, TreeShown: true, Themed: true}} {
		for _, c := range Commands() {
			rows := r.Palette(l, ctx, PaletteQuery{Text: c.Label}).Rows
			found := false
			for _, row := range rows {
				found = found || row.Command != nil && row.Command.ID == c.ID
			}
			if !found {
				t.Errorf("from %s, %q does not find %s: %s", name, c.Label, c.ID, paletteIDs(rows))
			}
		}
	}
}
