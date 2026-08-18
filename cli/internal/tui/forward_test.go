package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// forwardSandboxes is the listing with the first sandbox serving something, so
// the workspace's header has ports to draw arrows on.
func forwardSandboxes() []Sandbox {
	sandboxes := testSandboxes()
	sandboxes[0].Ports = []Port{
		{Number: 8080, Protocol: "http"},
		{Number: 5432, Protocol: "tcp"},
	}
	return sandboxes
}

// The workspace opens a forward with itself: the header lists what the
// discobox is serving, and the point is that those ports are reachable from
// here without asking for anything.
func TestWorkspaceOpensAForward(t *testing.T) {
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	ds.mu.Lock()
	opened := ds.forwardsOpen
	ds.mu.Unlock()
	if opened != 1 {
		t.Fatalf("forwards opened = %d, want 1", opened)
	}
}

// A port that gets bound while the screen is up appears on it without anything
// being pressed: the bind wakes the window, and the header redraws from the
// forward.
func TestWorkspaceHeaderShowsBoundPortsAsTheyArrive(t *testing.T) {
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	if frame := ansi.Strip(frameText(m)); !strings.Contains(frame, "http:8080") {
		t.Fatalf("the header should list the port before it is bound:\n%s", frame)
	}

	ds.forward.bind(Binding{Port: 8080, Local: 8082})
	ds.forward.bind(Binding{Port: 5432, Local: 5433})
	d.wait("the arrows", func() bool {
		return strings.Contains(ansi.Strip(frameText(m)), "http:8082->8080")
	})
	if frame := ansi.Strip(frameText(m)); !strings.Contains(frame, "tcp:5433->5432") {
		t.Fatalf("a forwarded tcp port should show its local port too:\n%s", frame)
	}
	// The web port is a link to the local end of the forward; the database is
	// not a link at all.
	if raw := rawFrame(m); !strings.Contains(raw, "http://localhost:8082") {
		t.Errorf("the forwarded http port should link to localhost:8082:\n%q", raw)
	}
	if raw := rawFrame(m); strings.Contains(raw, "localhost:5433") {
		t.Errorf("a forwarded tcp port should carry no link:\n%q", raw)
	}
}

// Detaching releases the local ports. They were taken to serve a screen that
// is gone, and nothing on screen would account for them.
func TestDetachingClosesTheForward(t *testing.T) {
	ds := newFakeSource(forwardSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the forward", func() bool { return m.forward != nil })

	d.key("ctrl+a")
	d.key("d")
	d.wait("the workspace to close", func() bool { return m.forward == nil })

	ds.mu.Lock()
	closed := ds.forwardsClose
	ds.mu.Unlock()
	if closed != 1 {
		t.Fatalf("forwards closed = %d, want 1", closed)
	}
}

// A forward that cannot be opened is reported and costs nothing else. The
// terminals are the screen; the ports ride on its header, which goes on saying
// what the discobox is serving with no arrows on it.
//
// The message is asserted on the model rather than the frame because the
// workspace screen draws hints where every other screen draws the status line
// — so it is read on the way back out, which is where the window can be acted
// on anyway.
func TestAForwardThatFailsLeavesTheWorkspaceOpen(t *testing.T) {
	ds := newFakeSource(forwardSandboxes()...)
	ds.forwardErr = errors.New("no route to the sandbox")
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the failure", func() bool {
		return strings.Contains(m.status, "ports are not being forwarded")
	})

	if m.terminal == nil {
		t.Fatal("the workspace should still be open")
	}
	if m.forward != nil {
		t.Fatal("a forward that failed should leave nothing to close")
	}
	frame := ansi.Strip(frameText(m))
	if !strings.Contains(frame, "http:8080") {
		t.Fatalf("the header should still list what the discobox is serving:\n%s", frame)
	}
	if strings.Contains(frame, "->8080") {
		t.Fatalf("nothing is forwarded, so no arrow should be drawn:\n%s", frame)
	}
}
