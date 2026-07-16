package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// press applies a key and returns the concrete screen.
func press(s *sandboxesScreen, spec string) *sandboxesScreen {
	next, _ := s.Update(keyPress(spec))
	return next.(*sandboxesScreen)
}

// enterTable steps off the "new" selector into the first table row.
func enterTable(s *sandboxesScreen) *sandboxesScreen { return press(s, "down") }

func TestStartsOnNewSelector(t *testing.T) {
	s := newTestScreen(&fakeSource{sandboxes: makeSandboxes(3)})
	if !s.newSelected {
		t.Fatal("screen should start on the new-session selector")
	}
}

func TestNavigationMovesCursor(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(5)}
	s := newTestScreen(f)

	// Down off the "new" selector lands on the first row.
	s = press(s, "down")
	if s.newSelected || s.table.Cursor() != 0 {
		t.Fatalf("after first down: newSelected=%v cursor=%d, want false/0", s.newSelected, s.table.Cursor())
	}

	// j moves further down.
	s = press(s, "j")
	if got := s.table.Cursor(); got != 1 {
		t.Fatalf("cursor after j = %d, want 1", got)
	}
	if sb, _ := s.cursorSandbox(); sb.ID != idFor(1) {
		t.Fatalf("cursorSandbox = %s, want %s", sb.ID, idFor(1))
	}

	// k at the top row steps back onto the "new" selector.
	s = press(s, "k")
	if s.newSelected {
		t.Fatal("k from row 1 should not select new yet")
	}
	s = press(s, "k")
	if !s.newSelected {
		t.Fatal("k from row 0 should select the new selector")
	}

	// G jumps to the bottom row, g back to the new selector.
	s = press(s, "G")
	if s.newSelected || s.table.Cursor() != 4 {
		t.Fatalf("after G: newSelected=%v cursor=%d, want false/4", s.newSelected, s.table.Cursor())
	}
	s = press(s, "g")
	if !s.newSelected {
		t.Fatal("g should select the new selector")
	}
}

func TestNewKeyOpensForm(t *testing.T) {
	s := newTestScreen(&fakeSource{sandboxes: makeSandboxes(2)})
	s = enterTable(s) // even off the selector, n opens the form
	_, cmd := s.Update(keyPress("n"))
	if _, ok := runCmd(cmd).(openNewMsg); !ok {
		t.Fatal("n did not emit openNewMsg")
	}
}

func TestEnterOnNewSelectorOpensForm(t *testing.T) {
	s := newTestScreen(&fakeSource{sandboxes: makeSandboxes(2)}) // starts on new
	_, cmd := s.Update(keyPress("enter"))
	if _, ok := runCmd(cmd).(openNewMsg); !ok {
		t.Fatal("enter on new selector did not emit openNewMsg")
	}
}

func TestMarkToggle(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))

	s.Update(keyPress("space")) // mark row 0
	if ids := s.markedIDs(); len(ids) != 1 || ids[0] != idFor(0) {
		t.Fatalf("marked = %v, want [%s]", ids, idFor(0))
	}
	s.Update(keyPress("space")) // unmark row 0
	if ids := s.markedIDs(); len(ids) != 0 {
		t.Fatalf("marked after toggle = %v, want empty", ids)
	}
}

func TestSelectAllTogglesEveryRow(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(4)}
	s := enterTable(newTestScreen(f))

	// ^a marks every sandbox.
	s = press(s, "ctrl+a")
	if ids := s.markedIDs(); len(ids) != 4 {
		t.Fatalf("select-all marked %d, want 4", len(ids))
	}
	// ^a again clears them.
	s = press(s, "ctrl+a")
	if ids := s.markedIDs(); len(ids) != 0 {
		t.Fatalf("second select-all marked %d, want 0", len(ids))
	}
}

func TestSelectAllIgnoredOnNewSelector(t *testing.T) {
	s := newTestScreen(&fakeSource{sandboxes: makeSandboxes(3)}) // on new
	s = press(s, "ctrl+a")
	if ids := s.markedIDs(); len(ids) != 0 {
		t.Fatalf("select-all on new selector marked %v, want none", ids)
	}
}

func TestConfirmDialogListsNames(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))
	s = press(s, "ctrl+a") // mark all
	s.Update(keyPress("d"))
	if !s.confirming {
		t.Fatal("expected confirmation dialog")
	}
	out := s.confirmDialog()
	for i := 0; i < 3; i++ {
		if !strings.Contains(out, nameFor(i)) {
			t.Fatalf("confirm dialog missing name %q:\n%s", nameFor(i), out)
		}
	}
}

func TestVisualRangeSelectCommits(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(5)}
	s := enterTable(newTestScreen(f))
	s = press(s, "down") // cursor -> row 1

	s = press(s, "v") // enter visual at row 1
	if !s.visual {
		t.Fatal("v did not enter visual mode")
	}
	s = press(s, "down") // extend to row 2
	s = press(s, "down") // extend to row 3

	want := map[string]bool{idFor(1): true, idFor(2): true, idFor(3): true}
	got := map[string]bool{}
	for _, id := range s.markedIDs() {
		got[id] = true
	}
	if len(got) != 3 || !got[idFor(1)] || !got[idFor(2)] || !got[idFor(3)] {
		t.Fatalf("visual marks = %v, want %v", got, want)
	}

	s = press(s, "v") // commit
	if s.visual {
		t.Fatal("second v did not commit")
	}
	if len(s.markedIDs()) != 3 {
		t.Fatalf("marks not kept after commit: %v", s.markedIDs())
	}
}

func TestVisualCancelRestoresMarks(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(5)}
	s := enterTable(newTestScreen(f))
	s.Update(keyPress("space")) // pre-mark row 0

	s = press(s, "down")         // row 1
	s = press(s, "v")            // visual from row 1
	s = press(s, "down")         // extend to row 2
	if len(s.markedIDs()) != 3 { // row0 (base) + rows 1,2
		t.Fatalf("during visual marks = %v, want 3", s.markedIDs())
	}

	s = press(s, "esc") // cancel restores base
	if s.visual {
		t.Fatal("esc did not exit visual")
	}
	ids := s.markedIDs()
	if len(ids) != 1 || ids[0] != idFor(0) {
		t.Fatalf("after cancel marks = %v, want [%s]", ids, idFor(0))
	}
}

func TestMarkIgnoredOnNewSelector(t *testing.T) {
	s := newTestScreen(&fakeSource{sandboxes: makeSandboxes(3)}) // on new
	s.Update(keyPress("space"))
	if ids := s.markedIDs(); len(ids) != 0 {
		t.Fatalf("space on new selector marked %v, want none", ids)
	}
}

func TestDeleteMarkedFlowConfirms(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))

	// Mark rows 0 and 1.
	s.Update(keyPress("space"))
	s = press(s, "down")
	s.Update(keyPress("space"))

	// d opens the confirmation without deleting yet.
	s.Update(keyPress("d"))
	if !s.confirming {
		t.Fatal("expected confirming after d")
	}
	if len(s.pending) != 2 {
		t.Fatalf("pending = %v, want 2 ids", s.pending)
	}
	if got := f.deletedIDs(); len(got) != 0 {
		t.Fatalf("delete happened before confirm: %v", got)
	}

	// y confirms and issues the delete command.
	_, cmd := s.Update(keyPress("y"))
	if s.confirming {
		t.Fatal("still confirming after y")
	}
	msg := runCmd(cmd)
	del, ok := msg.(deletedMsg)
	if !ok {
		t.Fatalf("delete cmd returned %T, want deletedMsg", msg)
	}
	if len(del.ids) != 2 {
		t.Fatalf("deletedMsg ids = %v, want 2", del.ids)
	}
	if got := f.deletedIDs(); len(got) != 2 {
		t.Fatalf("source deleted = %v, want 2", got)
	}
}

func TestDeleteFallsBackToCursorRow(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))

	s = press(s, "down") // cursor -> row 1
	s.Update(keyPress("d"))
	if len(s.pending) != 1 || s.pending[0].ID != idFor(1) {
		t.Fatalf("pending = %v, want [%s]", s.pending, idFor(1))
	}
}

func TestDeleteCancel(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))

	s.Update(keyPress("d"))
	_, cmd := s.Update(keyPress("n"))
	if s.confirming || s.pending != nil {
		t.Fatal("cancel did not clear confirm state")
	}
	if runCmd(cmd) != nil {
		t.Fatal("cancel issued a command")
	}
	if got := f.deletedIDs(); len(got) != 0 {
		t.Fatalf("cancel deleted rows: %v", got)
	}
}

func TestEnterEmitsSelect(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))

	s = press(s, "down") // row 1
	_, cmd := s.Update(keyPress("enter"))
	msg := runCmd(cmd)
	sel, ok := msg.(selectSandboxMsg)
	if !ok {
		t.Fatalf("enter returned %T, want selectSandboxMsg", msg)
	}
	if sel.sandbox.ID != idFor(1) {
		t.Fatalf("selected %s, want %s", sel.sandbox.ID, idFor(1))
	}
}

func TestFullscreenEmitsSelectedSandbox(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))

	s = press(s, "down") // row 1
	_, cmd := s.Update(keyPress("f"))
	msg := runCmd(cmd)
	fullscreen, ok := msg.(fullscreenSandboxMsg)
	if !ok {
		t.Fatalf("f returned %T, want fullscreenSandboxMsg", msg)
	}
	if fullscreen.sandbox.ID != idFor(1) {
		t.Fatalf("selected %s, want %s", fullscreen.sandbox.ID, idFor(1))
	}
}

func TestFullscreenDoesNothingOnNewSelector(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1)}
	s := newTestScreen(f)

	_, cmd := s.Update(keyPress("f"))
	if msg := runCmd(cmd); msg != nil {
		t.Fatalf("f on new selector returned %T, want nil", msg)
	}
}

func TestRefreshPreservesAndPrunesMarks(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := enterTable(newTestScreen(f))
	s.Update(keyPress("space")) // mark row 0 (idFor(0))

	// A refresh that still contains the marked sandbox keeps the mark.
	s.applySandboxes(makeSandboxes(3))
	if !s.marked[idFor(0)] {
		t.Fatal("mark dropped on refresh that still contains it")
	}

	// A refresh that no longer contains it prunes the mark.
	s.applySandboxes(makeSandboxes(3)[1:])
	if s.marked[idFor(0)] {
		t.Fatal("mark survived a refresh that removed the sandbox")
	}
}

func TestDeletedMsgClearsMarks(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}
	s := newTestScreen(f)
	s.marked[idFor(0)] = true

	next, _ := s.Update(deletedMsg{ids: []string{idFor(0)}, errs: []error{nil}})
	s = next.(*sandboxesScreen)
	if s.marked[idFor(0)] {
		t.Fatal("mark not cleared after successful delete")
	}
}

func TestQuitKey(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(1)}
	s := newTestScreen(f)
	_, cmd := s.Update(keyPress("q"))
	if _, ok := runCmd(cmd).(tea.QuitMsg); !ok {
		t.Fatal("q did not produce QuitMsg")
	}
}

// TestNewSelectorKeepsRowsAligned guards against the inactive-table Selected
// style padding (and thereby shifting) the cursor row when the "new" selector
// holds focus: every table row must have the same display width in both states.
func TestNewSelectorKeepsRowsAligned(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(3)}

	widths := func(s *sandboxesScreen) []int {
		lines := strings.Split(s.table.View(), "\n")
		out := make([]int, len(lines))
		for i, ln := range lines {
			out[i] = ansi.StringWidth(ln)
		}
		return out
	}

	// New selected (table inactive): row 0 must not be wider than the rest.
	s := newTestScreen(f)
	if !s.newSelected {
		t.Fatal("expected to start on the new selector")
	}
	w := widths(s)
	for i := 2; i < len(w); i++ {
		if w[1] != w[i] {
			t.Fatalf("row 0 width %d != row %d width %d while new selected", w[1], i-1, w[i])
		}
	}

	// Focused table: same invariant with the highlight applied.
	s = enterTable(s)
	w = widths(s)
	for i := 2; i < len(w); i++ {
		if w[1] != w[i] {
			t.Fatalf("row 0 width %d != row %d width %d while focused", w[1], i-1, w[i])
		}
	}
}

func TestViewRendersSandboxNames(t *testing.T) {
	f := &fakeSource{sandboxes: makeSandboxes(2)}
	s := newTestScreen(f)
	out := s.View(120, 20)
	if !strings.Contains(out, nameFor(0)) || !strings.Contains(out, nameFor(1)) {
		t.Fatalf("view missing sandbox names:\n%s", out)
	}
}

func TestSandboxTableUsesDisplayStateColumn(t *testing.T) {
	sandboxes := makeSandboxes(1)
	sandboxes[0].State = "stopping"
	s := newTestScreen(&fakeSource{sandboxes: sandboxes})
	out := s.View(120, 20)

	for _, want := range []string{"STATE", "stopping"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"PHASE", "DESIRED", "GEN"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("view contains removed column %q:\n%s", unwanted, out)
		}
	}
}

func TestViewEmptyState(t *testing.T) {
	f := &fakeSource{sandboxes: nil}
	s := newTestScreen(f)
	out := s.View(120, 20)
	if !strings.Contains(out, "no sandboxes") {
		t.Fatalf("expected empty-state hint, got:\n%s", out)
	}
}
