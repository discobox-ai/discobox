package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// A discobox whose create stopped after it parked and before its source was
// pushed waits for a push nobody is making. Attaching to it delivers first and
// attaches after (ADR 26-09-24-005).

// parkedSandbox is a discobox parked waiting for a source this machine can
// deliver.
func parkedSandbox() Sandbox {
	box := testSandboxes()[0]
	box.AwaitsDelivery = true
	return box
}

func TestAnAttachDeliversAParkedSourceBeforeItAttaches(t *testing.T) {
	t.Parallel()
	box := parkedSandbox()
	ds := newFakeSource(box)
	ds.deliverSteps = []string{"pushing source"}
	d, m := openAttached(t, ds, box)
	d.wait("the primary terminal", func() bool { return m.primary() != nil })

	ds.mu.Lock()
	delivered := slices.Clone(ds.delivered)
	opensAtDelivery := slices.Clone(ds.execOpensAtDelivery)
	ds.mu.Unlock()
	if !slices.Equal(delivered, []string{box.ID}) {
		t.Fatalf("delivered = %v, want the parked discobox delivered once", delivered)
	}
	if opensAtDelivery[0] != 0 {
		t.Fatalf("%d sessions were opened before the delivery, want it to come first", opensAtDelivery[0])
	}
}

// A discobox that is not parked is attached to as it always was: nothing is
// delivered, and nothing is asked.
func TestAnAttachToARunningDiscoboxDeliversNothing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m := openAttached(t, ds, testSandboxes()[0])
	d.wait("the primary terminal", func() bool { return m.primary() != nil })

	ds.mu.Lock()
	delivered := slices.Clone(ds.delivered)
	ds.mu.Unlock()
	if len(delivered) != 0 {
		t.Fatalf("delivered = %v, want nothing delivered to a discobox that is not waiting", delivered)
	}
}

// A delivery that cannot be made ends the attach with why, rather than
// attaching to a discobox that will only ever wait.
func TestAnAttachThatCannotDeliverEndsWithTheError(t *testing.T) {
	t.Parallel()
	box := parkedSandbox()
	ds := newFakeSource(box)
	ds.awaitedErr = errors.New("the commit the discobox is waiting for is gone")
	d, m := openAttached(t, ds, box)
	d.wait("the window to close", func() bool { return m.quit })

	if m.exitErr == nil || !strings.Contains(m.exitErr.Error(), "the commit the discobox is waiting for is gone") {
		t.Fatalf("exitErr = %v, want the delivery's own failure", m.exitErr)
	}
	ds.mu.Lock()
	opens := len(ds.execOpens)
	ds.mu.Unlock()
	if opens != 0 {
		t.Fatalf("%d sessions were opened, want none after a delivery that failed", opens)
	}
}

// From the list, a delivery that fails closes the workspace and says why,
// leaving the list where it was.
func TestAWorkspaceThatCannotDeliverReturnsToTheList(t *testing.T) {
	t.Parallel()
	box := parkedSandbox()
	ds := newFakeSource(box)
	ds.awaitedErr = errors.New("source primary came from /src/gone")
	m := New(t.Context(), ds)
	m.logo = logo{}
	m.copyOS = func(string) error { return nil }
	m.openOS = func(string) error { return nil }
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })

	d.key("tab")
	d.key("enter")
	d.wait("the report", func() bool { return strings.Contains(frameText(m), "/src/gone") })

	if m.primary() != nil || m.quit {
		t.Fatal("the workspace should close back to the list")
	}
}
