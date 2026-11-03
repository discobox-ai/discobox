package tui

import (
	"errors"
	"testing"
	"time"
)

// An archive pressed on the list takes effect on the row as soon as it is
// asked for — out of the list, as an archived box is, or marked as going —
// not only once the server has answered and the list has been read again.
func TestArchivingFromTheListShowsAtOnce(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.doGate = make(chan struct{})
	m := newTestModel(t, ds)
	d := newDriver(t, m)
	d.start()
	d.key("tab")
	box := m.list.current()
	if box == nil {
		t.Fatal("no row under the cursor")
	}
	d.key("x")
	d.wait("the archive asked", func() bool { return m.busy == "archive…" })

	for _, row := range m.list.rows() {
		if row.ID == box.ID && row.State != StateArchived {
			t.Errorf("row %s still listed as %q while its archive is out; want it gone or marked archiving at once", box.ID, row.State)
		}
	}
	close(ds.doGate)
	d.wait("the archive", func() bool { return m.busy == "" })
}

// archiveSent has the window send an archive for one box, with the cursor on
// another, without running it, so a test can say what lands, and in what
// order, while it is out.
func archiveSent(t *testing.T, ds *fakeSource, id, cursorOn string) *Model {
	t.Helper()
	m := newTestModel(t, ds)
	send(t, m, listLoadedMsg{listing: Listing{Sandboxes: testSandboxes()}})
	m.list.folder = folder{} // every folder, so there are rows on both sides
	if !m.listCursorTo(cursorOn) {
		t.Fatalf("%s is not listed", cursorOn)
	}
	_ = m.runVerb(VerbArchive, []string{id}, verbThen{})
	if got := m.list.current(); got == nil || got.ID != cursorOn {
		t.Fatalf("the cursor is on %v once the archive went out, want it kept on %s", got, cursorOn)
	}
	return m
}

func listedState(m *Model, id string) (State, bool) {
	for _, s := range m.list.all {
		if s.ID == id {
			return s.State, m.list.shown(s)
		}
	}
	return "", false
}

// landed hands the window the listing out, without running what it asks for
// next: a read owed behind it would otherwise land first and answer for it.
// What it asks for next is still sent, as the window sends it.
func landed(m *Model, boxes []Sandbox) {
	m.Update(listLoadedMsg{listing: Listing{Sandboxes: boxes}})
}

// A listing sent before the archive was answered still says the box is
// running; it does not put the row back while the archive is out, nor once
// the archive has been answered. The listing sent after the answer is the
// server's word, and settles it. Which is which is counted in reads sent, so
// it holds on a clock that cannot tell the two moments apart.
func TestAListingOlderThanTheArchiveDoesNotBringTheRowBack(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := archiveSent(t, ds, "sbx_one", "sbx_two")

	if !m.listPoll.start(m.now()) {
		t.Fatal("a read was already out")
	}
	landed(m, testSandboxes())
	if st, shown := listedState(m, "sbx_one"); st != StateArchived || shown {
		t.Fatalf("while the archive is out a stale listing put the row back: state %q, shown %v", st, shown)
	}

	// Answered while a read sent before it is still out: that read, landing,
	// still says running. The answer's own read waits behind it.
	if !m.listPoll.start(m.now()) {
		t.Fatal("a read was already out")
	}
	m.Update(verbDoneMsg{verb: VerbArchive, ids: []string{"sbx_one"}, errs: []error{nil}})
	landed(m, testSandboxes())
	if st, shown := listedState(m, "sbx_one"); st != StateArchived || shown {
		t.Fatalf("a listing sent before the answer put the row back: state %q, shown %v", st, shown)
	}
	if got := m.list.current(); got == nil || got.ID != "sbx_two" {
		t.Fatalf("the cursor is on %v, want it kept on sbx_two", got)
	}
	if m.listPoll.since.IsZero() {
		t.Fatal("the answer's own read was not sent once the stale one landed")
	}

	// The read sent after the answer has the server's word: archiving,
	// which the window lists as archived.
	after := testSandboxes()
	after[0].State = StateArchived
	landed(m, after)
	if len(m.list.archiving) != 0 {
		t.Errorf("archive marks = %v once a listing after the answer landed, want none", m.list.archiving)
	}
}

// An archive the server refuses puts the row back as it was, and the cursor
// stays on the box it was on, rather than leaving a running box hidden as
// though archived.
func TestAFailedArchivePutsTheRowBack(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := archiveSent(t, ds, "sbx_one", "sbx_two")
	if _, shown := listedState(m, "sbx_one"); shown {
		t.Fatal("the row is still listed while its archive is out")
	}

	m.listPoll.since = time.Now() // hold the answer's own refresh back
	m.Update(verbDoneMsg{verb: VerbArchive, ids: []string{"sbx_one"}, errs: []error{errors.New("server said no")}})
	if st, shown := listedState(m, "sbx_one"); st != StateRunning || !shown {
		t.Fatalf("after a failed archive the row is %q, shown %v; want it back as running", st, shown)
	}
	if got := m.list.current(); got == nil || got.ID != "sbx_two" {
		t.Fatalf("the cursor is on %v after the row came back, want it kept on sbx_two", got)
	}
	if len(m.list.archiving) != 0 {
		t.Errorf("archive marks = %v after a failed archive, want none", m.list.archiving)
	}
}
