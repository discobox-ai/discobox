package tui

import (
	"errors"
	"strings"
	"testing"
)

// Ctrl-O opens the options, the Source row cycles in place, and Enter opens
// the whole list — the same two affordances the header's folder filter has,
// because they are the same control.
func TestTheSourceRowCyclesAndOpensItsList(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, keyPress("ctrl+o"))
	if !m.optionsOpen {
		t.Fatal("ctrl+o should open the run options")
	}
	for m.opts.cursor != optSource {
		send(t, m, keyPress("down"))
	}
	// Right moves to the next source, and the list follows it to the folder
	// that source's discoboxes are filed under — the header and the row are one
	// control, so neither is left saying something the other contradicts.
	send(t, m, keyPress("right"))
	if got := m.opts.opts[optSource].selected(); got != "/src/obot" {
		t.Fatalf("source = %q, want the next one the project holds", got)
	}
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the list to have followed", m.list.folder)
	}
	// Right again reaches the third entry rather than bouncing back: the row's
	// order does not move under the cursor when the folder follows it.
	send(t, m, keyPress("right"))
	if got := m.opts.opts[optSource].selected(); got != "https://github.com/acme/foo" {
		t.Fatalf("source = %q, want the third entry", got)
	}
	send(t, m, keyPress("right"))
	if got := m.opts.opts[optSource].selected(); got != sourceNone {
		t.Fatalf("source = %q, want no source at the end of the row", got)
	}
	send(t, m, keyPress("right"))

	send(t, m, keyPress("enter"))
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

	send(t, m, keyPress("ctrl+o"))
	for m.opts.cursor != optSource {
		send(t, m, keyPress("down"))
	}
	send(t, m, keyPress("enter"), keyPress("e"))
	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("the last row should open the input field")
	}
	for _, r := range "/src/typed@wip" {
		send(t, m, keyPress(string(r)))
	}
	send(t, m, keyPress("enter"))

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
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("right"))
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

// "All folders" is the one header choice that is not a place, so Enter has no
// folder to cut from. It asks rather than falling back to the directory the
// window happens to be running in, which is a discobox cut from somewhere
// nobody named.
func TestCreatingWithEveryFolderShownAsksWhereToCutFrom(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	showAllFolders(t, m)
	send(t, m, keyPress("esc"))
	send(t, m, typeString("fix the reaper")...)
	send(t, m, keyPress("enter"))

	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("a create with no folder to cut from should ask which source to use")
	}
	if len(ds.runs) != 0 {
		t.Fatalf("runs = %v, want the create held until the question is answered", ds.runs)
	}
	body := strings.Join(dialogLabels(m.dialog), "\n")
	for _, want := range []string{"/src/disco2", "/src/obot", noSourceChoice, enterSourceChoice} {
		if !strings.Contains(body, want) {
			t.Fatalf("the question is missing %q:\n%s", want, body)
		}
	}

	// Answering it creates: the answer is what the run was waiting for, and it
	// is the same answer the Source row takes, so the header moves onto that
	// folder and the next Enter asks nothing.
	send(t, m, keyPress("2"))
	if len(ds.runs) != 1 {
		t.Fatalf("runs = %v, want the create the question interrupted", ds.runs)
	}
	if got := ds.runs[0].Source; got != "/src/obot" {
		t.Fatalf("source = %q, want the one that was chosen", got)
	}
	if got := promptText(ds.runs[0]); got != "fix the reaper" {
		t.Fatalf("prompt = %q, want the one that was waiting", got)
	}
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the header to have followed the answer", m.list.folder)
	}
}

// The answer can be that there is nothing to cut from, which is a discobox with
// an empty workspace rather than one cut from the wrong place.
func TestTheCreateQuestionTakesNoSourceForAnAnswer(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	showAllFolders(t, m)
	send(t, m, keyPress("esc"), keyPress("enter"))

	labels := dialogLabels(m.dialog)
	key := ""
	for i, label := range labels {
		if label == noSourceChoice {
			key = itoa(i + 1)
		}
	}
	if key == "" {
		t.Fatalf("the question should offer %q: %v", noSourceChoice, labels)
	}
	send(t, m, keyPress(key))

	if len(ds.runs) != 1 || !ds.runs[0].NoSource {
		t.Fatalf("runs = %v, want one discobox created with nothing checked out", ds.runs)
	}
}

// A path typed by hand is vouched for by nothing, so it is checked before the
// window takes it: the create is the wrong place to find out, since it fails
// seconds later with the field gone and the path to retype from memory.
func TestATypedSourceThatIsNotThereGoesBackIntoTheField(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.sourceErr = errors.New("/src/typo does not exist")
	m := newTestModel(t, ds)

	send(t, m, keyPress("ctrl+o"))
	for m.opts.cursor != optSource {
		send(t, m, keyPress("down"))
	}
	send(t, m, keyPress("enter"), keyPress("e"))
	send(t, m, typeString("/src/typo")...)
	send(t, m, keyPress("enter"))

	if m.dialog == nil || m.dialog.kind != dlgInput {
		t.Fatal("a path that is not there should come back to the field it was typed into")
	}
	if !m.dialog.err || !strings.Contains(m.dialog.body, "does not exist") {
		t.Fatalf("the field should say what is wrong: err=%v body=%q", m.dialog.err, m.dialog.body)
	}
	if got := m.dialog.input.Value(); got != "/src/typo" {
		t.Fatalf("field = %q, want what was typed, to correct rather than retype", got)
	}
	if got := m.opts.opts[optSource].selected(); got != "/src/disco2" {
		t.Fatalf("source = %q, want the refused path not taken", got)
	}

	// Corrected, it is taken: the check is on what the field holds now, not on
	// the answer it was refused for.
	ds.setSourceErr(nil)
	send(t, m, keyPress("enter"))
	if got := m.opts.opts[optSource].selected(); got != "/src/typo" {
		t.Fatalf("source = %q, want the corrected path", got)
	}
}

// The field is also how the create's own question is answered with a place the
// project has never been cut from, and a path that checks out runs.
func TestAPathTypedIntoTheCreateQuestionRunsWhenItChecksOut(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	showAllFolders(t, m)
	send(t, m, keyPress("esc"))
	send(t, m, typeString("fix the reaper")...)
	send(t, m, keyPress("enter"), keyPress("e"))
	send(t, m, typeString("/src/elsewhere")...)
	send(t, m, keyPress("enter"))

	if got := ds.resolved; len(got) != 1 || got[0] != "/src/elsewhere" {
		t.Fatalf("resolved = %v, want the typed path checked before it was taken", got)
	}
	if len(ds.runs) != 1 {
		t.Fatalf("runs = %v, want the create the question interrupted", ds.runs)
	}
	if got := ds.runs[0].Source; got != "/src/elsewhere" {
		t.Fatalf("source = %q, want the path that was typed", got)
	}
	if got := promptText(ds.runs[0]); got != "fix the reaper" {
		t.Fatalf("prompt = %q, want the one that was waiting", got)
	}
}
