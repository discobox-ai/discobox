package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func taggedSandboxes() []Sandbox {
	boxes := testSandboxes()
	boxes[0].Tags = []string{"ticket=ENG-12", "wip"}
	boxes[1].Tags = []string{"wip"}
	return boxes
}

func headerLine(m *Model) string { return ansi.Strip(m.viewHeaderLeft()) }

// A project nobody tags has no tag filter: a control that can only say "all
// tags" is a control spent saying nothing.
func TestThereIsNoTagFilterUntilSomethingIsTagged(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	if m.showsTagFilter() {
		t.Fatal("the tag filter is offered with no tags")
	}
	if strings.Contains(headerLine(m), allTags) {
		t.Fatalf("header = %q, want no tag filter", headerLine(m))
	}
	// Tab from the folder goes on round the ring, past where the filter would be.
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("tab"))
	if m.focus == focusTags {
		t.Fatal("Tab reached a tag filter that is not there")
	}
}

// Once a discobox is tagged the header offers the filter after the folder, Tab
// reaches it from the folder, and each tag narrows the list to the boxes
// carrying it.
func TestTheTagFilterNarrowsTheList(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	if !strings.Contains(headerLine(m), allTags) {
		t.Fatalf("header = %q, want the tag filter", headerLine(m))
	}
	if got := m.tagChoices(); len(got) != 3 || got[0] != "" || got[1] != "ticket=ENG-12" || got[2] != "wip" {
		t.Fatalf("choices = %q, want every box then each tag in order", got)
	}

	send(t, m, keyPress("tab"), keyPress("up"), keyPress("tab"))
	if m.focus != focusTags {
		t.Fatalf("focus = %v, want the tag filter after the folder", m.focus)
	}
	rows := func() []string {
		var ids []string
		for _, s := range m.list.rows() {
			ids = append(ids, s.ID)
		}
		return ids
	}
	if got := rows(); len(got) != 2 {
		t.Fatalf("rows = %v, want both of this folder's boxes", got)
	}

	send(t, m, keyPress("right"))
	if m.list.tag != "ticket=ENG-12" || !strings.Contains(headerLine(m), "#ticket=ENG-12") {
		t.Fatalf("tag = %q, header = %q", m.list.tag, headerLine(m))
	}
	if got := rows(); len(got) != 1 || got[0] != "sbx_one" {
		t.Fatalf("rows = %v, want only the box with that tag", got)
	}
	send(t, m, keyPress("right"))
	if got := rows(); m.list.tag != "wip" || len(got) != 2 {
		t.Fatalf("tag %q lists %v, want both boxes tagged wip", m.list.tag, got)
	}
	send(t, m, keyPress("right"))
	if m.list.tag != "" {
		t.Fatalf("tag = %q, want the ring back at every box", m.list.tag)
	}

	// Down drops into the list, the way it does from the folder.
	send(t, m, keyPress("down"))
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// The dropdown lists every tag with how many boxes carry it, and choosing one
// applies it.
func TestTheTagDropdownCountsAndChooses(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("tab"), keyPress("enter"))
	if m.dialog == nil {
		t.Fatal("enter on the tag filter should open the dropdown")
	}
	view := m.dialog.view(m.st, &m.zones, 120, 40)
	for _, want := range []string{allTags, "#ticket=ENG-12", "#wip", "2 boxes"} {
		if !strings.Contains(view, want) {
			t.Errorf("the dropdown is missing %q:\n%s", want, view)
		}
	}
	send(t, m, keyPress("down"), keyPress("down"), keyPress("enter"))
	if m.list.tag != "wip" {
		t.Fatalf("tag = %q, want the choice that was made", m.list.tag)
	}
}

// A tag that goes while the filter is on it stays the filter's choice rather
// than vanishing from under it, so what the header says is still what is shown.
func TestAChosenTagOutlivesItsLastBox(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(taggedSandboxes()...))
	m.list.tag = "wip"
	untagged := testSandboxes()
	m.list.setAll(untagged)
	if !m.showsTagFilter() || !strings.Contains(headerLine(m), "#wip") {
		t.Fatalf("header = %q, want the filter still naming #wip", headerLine(m))
	}
	if len(m.list.rows()) != 0 {
		t.Fatalf("rows = %d, want none: nothing carries the tag now", len(m.list.rows()))
	}
}
