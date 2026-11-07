package tui

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

func TestPaletteMatchesFieldsAndRanks(t *testing.T) {
	tests := []struct {
		query           string
		primary, detail []string
		want            bool
		tier            int
	}{
		{"hcp", []string{"sbx_hcp123"}, nil, true, 2},
		{"hcp", []string{"hcp123"}, nil, true, 1},
		{"hcp", []string{"hcp"}, nil, true, 0},
		{"hcp", []string{"h---c---p"}, nil, true, 3},
		{"hcp", nil, []string{"issue=hcp"}, true, 4},
		{"Repository 717", []string{"box"}, []string{"Repository", "issue=717"}, true, 4},
		{"Repository", nil, []string{"repository"}, false, 0},
		{"ab", []string{"a", "b"}, nil, false, 0},
		{"hcp missing", []string{"hcp"}, nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got, ok := paletteMatch(parseFuzzy(tt.query), tt.primary, tt.detail)
			if ok != tt.want || ok && got.tier != tt.tier {
				t.Fatalf("got %+v %v, want tier %d %v", got, ok, tt.tier, tt.want)
			}
		})
	}
}

func TestPaletteChipOrdersAndEscape(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		m := newTestModel(t, ds)
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		m.focus = focusList
		d.key(":")
		if m.palette == nil {
			t.Fatal("colon did not open palette")
		}
		d.dispatch(tea.PasteMsg{Content: "stop sbx"})
		d.settle()
		if m.palette.command != "stop" || m.palette.input.Value() != "sbx" {
			t.Fatalf("pasted completion: %+v", m.palette)
		}
		for _, r := range m.palette.rows {
			if r.box == nil || r.box.State != console.StateRunning && r.box.State != console.StateSuspended {
				t.Fatalf("stop offers %+v", r)
			}
		}
		d.key("esc")
		if m.palette.command != "" || m.palette.input.Value() != "sbx" {
			t.Fatal("escape did not retain query")
		}
		d.key("esc")
		if m.palette.input.Value() != "" {
			t.Fatal("escape did not clear query")
		}
		if m.palette.rows[0].box == nil {
			t.Fatal("current box is not first")
		}
		d.key("tab")
		if m.palette.box == nil {
			t.Fatal("tab did not chip box")
		}
		if slices.ContainsFunc(m.palette.rows, func(r paletteRow) bool { return r.command == nil || !r.command.targeted }) {
			t.Fatal("box chip offers global results")
		}
		d.key("esc")
		d.key("esc")
		if m.palette != nil {
			t.Fatal("empty escape did not close")
		}
	})
}

func TestPaletteKeepsDisappearedSelectionAndConsumesOutsideClick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newTestModel(t, newFakeSource(testSandboxes()...))
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		m.focus = focusList
		d.key(":")
		m.palette.input.SetValue("sbx_one")
		m.rebuildPalette(true)
		if len(m.palette.rows) == 0 {
			t.Fatal("no selected box")
		}
		m.list.all = slices.DeleteFunc(m.list.all, func(s console.Sandbox) bool { return s.ID == "sbx_one" })
		d.key("enter")
		if m.palette == nil || !strings.Contains(m.palette.reason, "no longer") {
			t.Fatal("missing target was replaced")
		}
		d.dispatch(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
		d.settle()
		if m.palette != nil {
			t.Fatal("outside click did not dismiss")
		}
	})
}

func TestPaletteLeaderFromPane(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource(testSandboxes()...)
		d, m, term := openWorkspace(t, ds, "enter")
		d.key("ctrl+a")
		d.key("k")
		d.wait("palette", func() bool { return m.palette != nil })
		d.key("esc")
		if m.palette != nil {
			t.Fatal("palette did not close")
		}
		if term.typed("k") == "k" {
			t.Fatal("palette opener reached application")
		}
	})
}

func TestPaletteFitsSmallScreens(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {160, 48}, {40, 12}} {
		synctest.Test(t, func(t *testing.T) {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			d := newDriver(t, m)
			d.startAt(size[0], size[1])
			d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
			m.focus = focusList
			d.key(":")
			frame := plainFrame(m)
			if lines := strings.Split(frame, "\n"); len(lines) > size[1] {
				t.Fatalf("%dx%d: %d rows", size[0], size[1], len(lines))
			}
			if !strings.Contains(frame, "Command palette") {
				t.Fatal("card missing")
			}
		})
	}
}

func TestPaletteMouseCanChooseBoxCommandsAndDropChip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newTestModel(t, newFakeSource(testSandboxes()...))
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		m.focus = focusList
		d.key(":")
		m.View()
		control, ok := m.zones.find(hitPaletteCommands)
		if !ok {
			t.Fatal("missing box commands control")
		}
		d.dispatch(tea.MouseClickMsg{X: control.x, Y: control.y, Button: tea.MouseLeft})
		d.settle()
		if m.palette == nil || m.palette.box == nil {
			t.Fatal("box commands click did not compose")
		}
		m.View()
		chip, ok := m.zones.find(hitPaletteChip)
		if !ok {
			t.Fatal("missing chip control")
		}
		d.dispatch(tea.MouseClickMsg{X: chip.x, Y: chip.y, Button: tea.MouseLeft})
		d.settle()
		if m.palette.box != nil {
			t.Fatal("chip click did not drop target")
		}
	})
}

func TestPaletteNavigationTemporarilyRevealsFilteredBox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		boxes := testSandboxes()
		boxes[1].State = console.StateStopped
		m := newTestModel(t, newFakeSource(boxes...))
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		d.settle()
		m.list.tags = []string{"missing-tag"}
		original := m.currentView()

		m.focus = focusList
		d.key(":")

		m.palette.input.SetValue("sbx_two")
		m.rebuildPalette(true)

		d.key("enter")
		if m.paletteReveal == nil {
			t.Fatalf("filtered result was not revealed: palette=%+v tags=%v focus=%v status=%s rows=%+v", m.palette, m.list.tags, m.focus, m.status, m.list.rows())
		}
		if _, ok := m.viewToSave(); ok {
			t.Fatal("temporary reveal would be saved")
		}
		if cur := m.list.current(); cur == nil || cur.ID != "sbx_two" {
			t.Fatalf("cursor = %+v", cur)
		}
		m.backToPrompt()
		if !m.currentView().Equal(original) {
			t.Fatal("leaving the result did not restore filters")
		}
	})
}

func TestPaletteControlsUseScreenCoordinatesBesideTree(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, m, _ := openWorkspace(t, newFakeSource(testSandboxes()...), "enter")
		d.dispatch(tea.WindowSizeMsg{Width: 160, Height: 48})
		d.settle()
		d.leader(paletteKey)
		d.wait("palette", func() bool { return m.palette != nil })
		m.View()
		control, ok := m.zones.find(hitPaletteCommands)
		if !ok {
			t.Fatal("no commands control")
		}
		d.dispatch(tea.MouseClickMsg{X: control.x, Y: control.y, Button: tea.MouseLeft})
		d.settle()
		if m.palette == nil || m.palette.box == nil {
			t.Fatal("tree translated the palette press")
		}
		d.dispatch(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
		d.settle()
		if m.palette != nil {
			t.Fatal("outside press over tree did not dismiss")
		}
	})
}

func TestPaletteBareStatusHasColorlessModalProvenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, m, _ := openBare(t, newFakeSource(testSandboxes()...))
		before := bareStatus(m)
		d.leader(paletteKey)
		d.wait("palette", func() bool { return m.palette != nil })
		during := bareStatus(m)
		if before == during || !strings.Contains(during, "Command palette") {
			t.Fatalf("bare row unchanged or not labeled: %q -> %q", before, during)
		}
		d.key("esc")
		if got := bareStatus(m); got != before {
			t.Fatalf("bare status was not restored: %q", got)
		}
	})
}

func TestPaletteLongQueryKeepsInputAndHitRowsAligned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		box := testSandboxes()[0]
		box.Description = strings.Repeat("a", 300)
		m := newTestModel(t, newFakeSource(box))
		d := newDriver(t, m)
		d.startAt(80, 24)
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		d.settle()
		m.focus = focusList
		d.key(":")
		d.dispatch(tea.PasteMsg{Content: strings.Repeat("a", 200)})
		d.settle()
		m.View()
		row, ok := m.zones.find(hitPaletteRow)
		if !ok {
			t.Fatal("no matching row")
		}
		lines := strings.Split(plainFrame(m), "\n")
		if !strings.Contains(lines[row.y], "□") {
			t.Fatalf("result drawn off its hit row: %q", lines[row.y])
		}
		d.dispatch(tea.MouseClickMsg{X: row.x, Y: row.y, Button: tea.MouseLeft})
		d.settle()
		if m.palette.selected != row.what.url {
			t.Fatal("wrong result selected")
		}
	})
}

func TestPaletteColorlessMarkerAndStaleListings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newTestModel(t, newFakeSource(testSandboxes()...))
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		d.settle()
		m.focus = focusList
		d.key(":")
		m.list.listed = map[string]time.Time{"hidden-server": m.list.now().Add(-time.Minute)}
		before := plainFrame(m)
		if !strings.Contains(before, "hidden-server listed") {
			t.Fatal("stale server outside filters was omitted")
		}
		if !strings.Contains(before, "❯") {
			t.Fatal("no colorless cursor")
		}
		selected := m.palette.selected
		d.key("down")
		if m.palette.selected == selected {
			t.Fatal("cursor did not move")
		}
	})
}

func TestPaletteLiveNavigationKeepsTemporaryReveal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newTestModel(t, newFakeSource(testSandboxes()...))
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		d.settle()
		m.list.tags = []string{"missing-tag"}
		original := m.currentView()
		m.focus = focusList
		d.key(":")
		m.palette.input.SetValue("sbx_two")
		m.rebuildPalette(true)
		d.key("enter")
		d.wait("workspace", func() bool { return m.primary() != nil && m.paneBox.ID == "sbx_two" })
		if m.paletteReveal == nil {
			t.Fatal("opening the live result restored the filters too early")
		}
		m.closeWorkspace()
		if !m.currentView().Equal(original) {
			t.Fatal("leaving result did not restore filters")
		}
		m.focus = focusList
		d.key(":")
		m.palette.input.SetValue("sbx_two")
		m.rebuildPalette(true)
		d.key("enter")
		d.wait("reopened workspace", func() bool { return m.primary() != nil })
		if m.paletteReveal == nil {
			t.Fatal("reopening detached result lost reveal")
		}

	})
}

func TestPaletteNewShortcutKeepsAttachAndNarrowWindowOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, m, _ := openBare(t, newFakeSource(testSandboxes()...))
		d.leader("n")
		if m.newOpen || !m.oneShot() || m.paneBox.ID != "sbx_one" {
			t.Fatal("leader-n escaped the one-shot window")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		d, m, _ := openNarrowWorkspace(t, newFakeSource(testSandboxes()...), "enter")
		d.leader("n")
		if m.newOpen {
			t.Fatal("leader-n opened a card without its tree")
		}
	})
}

func TestPaletteCountsShortcutUseAtFlowEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, m, _ := openWorkspace(t, newFakeSource(testSandboxes()...), "enter")
		d.leader("t")
		d.wait("stop counted", func() bool { return m.paletteUsage["stop"] > 0 })
		if m.paletteUsage["stop"] != 1 {
			t.Fatalf("stop counted %d times", m.paletteUsage["stop"])
		}
	})
}

func TestPaletteRoutesSameIDLifecycleAndWorkspacesToTheirServers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, b := testSandboxes()[0], testSandboxes()[0]
		a.Server = "alpha"
		b.Server = "beta"
		a.Name = "alpha worker"
		b.Name = "beta worker"
		alpha, beta := newFakeSource(a), newFakeSource(b)
		root := newFakeSource(a, b)
		root.session.Servers = []string{"alpha", "beta"}
		root.serverSources = map[string]*fakeSource{"alpha": alpha, "beta": beta}
		m := newTestModel(t, root)
		d := newDriver(t, m)
		d.start()
		d.wait("list", func() bool { return m.list.loaded && m.sessionRead })
		d.settle()
		m.focus = focusList
		d.key(":")
		d.dispatch(tea.PasteMsg{Content: "stop beta"})
		d.settle()
		d.key("enter")
		d.wait("stop beta", func() bool { return len(beta.verbs()) == 1 && m.busy == "" })
		if len(alpha.verbs()) != 0 {
			t.Fatal("beta stop reached alpha")
		}
		// Navigate to alpha and beta even though their full IDs are identical.
		m.focus = focusList
		d.key(":")
		m.palette.input.SetValue("alpha")
		m.rebuildPalette(true)
		d.key("enter")
		d.wait("alpha workspace", func() bool { return m.primary() != nil && m.paneBox.Server == "alpha" })
		d.leader(paletteKey)
		m.palette.input.SetValue("beta")
		m.rebuildPalette(true)
		d.key("enter")
		d.wait("beta workspace", func() bool { return m.primary() != nil && m.paneBox.Server == "beta" })
		if m.parked[console.BoxRef{Server: "alpha", ID: a.ID}] == nil {
			t.Fatal("switching same ID across servers lost the alpha workspace")
		}
	})
}

func TestPaletteReservesWorkspaceShortcutsAtEightyColumns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, m, _ := openNarrowWorkspace(t, newFakeSource(testSandboxes()...), "enter")
		d.dispatch(tea.WindowSizeMsg{Width: 80, Height: 24})
		d.settle()
		d.leader(paletteKey)
		m.palette.input.SetValue("settings")
		m.rebuildPalette(true)
		if frame := plainFrame(m); !strings.Contains(frame, "[ctrl+a E]") {
			t.Fatalf("settings shortcut was truncated:\n%s", frame)
		}
	})
}
