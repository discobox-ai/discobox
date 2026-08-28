package tui

import (
	"strings"
	"testing"
)

// Shift-Tab opens the options, the Source row cycles in place, and Enter opens
// the whole list — the same two affordances the header's folder filter has,
// because they are the same control.
func TestTheSourceRowCyclesAndOpensItsList(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, key("shift+tab"))
	if !m.optionsOpen {
		t.Fatal("shift+tab should open the run options")
	}
	for m.opts.cursor != optSource {
		send(t, m, key("down"))
	}
	// Right moves to the next source, and the list follows it to the folder
	// that source's discoboxes are filed under — the header and the row are one
	// control, so neither is left saying something the other contradicts.
	send(t, m, key("right"))
	if got := m.opts.opts[optSource].selected(); got != "/src/obot" {
		t.Fatalf("source = %q, want the next one the project holds", got)
	}
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the list to have followed", m.list.folder)
	}
	// Right again reaches the third entry rather than bouncing back: the row's
	// order does not move under the cursor when the folder follows it.
	send(t, m, key("right"))
	if got := m.opts.opts[optSource].selected(); got != "https://github.com/acme/foo" {
		t.Fatalf("source = %q, want the third entry", got)
	}
	send(t, m, key("right"))
	if got := m.opts.opts[optSource].selected(); got != sourceNone {
		t.Fatalf("source = %q, want no source at the end of the row", got)
	}
	send(t, m, key("right"))

	send(t, m, key("enter"))
	if m.dialog == nil {
		t.Fatal("enter on the Source row should open the whole list")
	}
	body := strings.Join(dialogLabels(m.dialog), "\n")
	for _, want := range []string{"/src/obot", "https://github.com/acme/foo", noSourceChoice, enterSourceChoice} {
		if !strings.Contains(body, want) {
			t.Fatalf("the dropdown is missing %q:\n%s", want, body)
		}
	}
}

func dialogLabels(d *dialog) []string {
	out := make([]string, 0, len(d.items))
	for _, item := range d.items {
		out = append(out, item.label)
	}
	return out
}

// The dropdown's last row is the one entry that is not a source: it opens the
// field where a path the listing has never seen is typed, ref and all.
func TestTheSourceDropdownTakesAPathOfYourOwn(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, key("shift+tab"))
	for m.opts.cursor != optSource {
		send(t, m, key("down"))
	}
	send(t, m, key("enter"), key("e"))
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("the last row should open the input field")
	}
	for _, r := range "/src/typed@wip" {
		send(t, m, key(string(r)))
	}
	send(t, m, key("enter"))

	if got := m.opts.request("").Source; got != "/src/typed@wip" {
		t.Fatalf("source = %q, want what was typed", got)
	}
	// The ref is not a folder, so the list follows the directory half of it.
	if m.list.folder != "/src/typed" {
		t.Fatalf("folder = %q, want the directory the ref sits in", m.list.folder)
	}
	// And it is worth a chip: the header names the directory, and the ref is
	// the part of the answer the window is not otherwise saying.
	if chips := m.opts.chips(m.st); !strings.Contains(chips, "/src/typed@wip") {
		t.Fatalf("chips = %q, want the ref on the strip", chips)
	}
}

// Moving the header moves the source with it, which is the direction that was
// always there: the header is where the folder is chosen.
func TestTheHeaderStillMovesTheSource(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	m.opts.chooseSource("https://github.com/acme/foo")
	send(t, m, key("tab"), key("up"), key("right"))
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the header to have moved", m.list.folder)
	}
	if got := m.opts.opts[optSource].selected(); got != "/src/obot" {
		t.Fatalf("source = %q, want the folder the header moved to", got)
	}
	if m.opts.opts[optSource].changed() {
		t.Fatal("the header already names it, so the panel has nothing to add")
	}
}
