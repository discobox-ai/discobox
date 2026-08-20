package tui

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func serviceExecRecord(id, service, name string) Exec {
	return Exec{
		ID:          id,
		Command:     []string{"/bin/bash", "-lc", "'/src/.discobox/services/10-api.sh'"},
		Service:     service,
		ServiceName: name,
		// A service runs on pipes: its output is read rather than typed at.
		Tty:       false,
		Live:      true,
		CreatedAt: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	}
}

// A running service is a tab on the right, drawn from the same listing every
// other session comes from — even though it is not a TTY session, which is the
// one place the tab strip widens past ADR 0054 §2.
func TestARunningServiceIsATabOnTheRight(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "discobox-api", "Discobox API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.shells.len() == 1 })

	if m.terminals.len() != 1 {
		t.Fatalf("terminals = %d, want just the primary", m.terminals.len())
	}
	p := m.shells.panes[0]
	if p.execID != "exec_svc1" {
		t.Fatalf("tab = %q, want the service's exec", p.execID)
	}
	if p.service != "discobox-api" {
		t.Errorf("pane service = %q, want discobox-api", p.service)
	}
	if p.action != InteractService {
		t.Errorf("action = %q, want %q", p.action, InteractService)
	}
	// The tab wears the service's name, which is the one thing about it a
	// person chose; its argv is the login shell every service shares.
	if got := p.name(); got != "Discobox API" {
		t.Errorf("tab name = %q, want %q", got, "Discobox API")
	}
	if !strings.Contains(plainFrame(m), "Discobox API") {
		t.Errorf("the strip should name the service:\n%s", plainFrame(m))
	}
}

// Nothing reads a service's stdin, so nothing types at its pane.
func TestAServicePaneIsReadOnly(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "discobox-api", "Discobox API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.shells.len() == 1 })

	// Move onto the tab and type at it.
	d.key("ctrl+a")
	d.key(paneRightKey)
	d.wait("focus on the service", func() bool { return m.onShells })
	d.key("h")
	d.key("i")

	// typed polls for the text, so it is the wait as well as the assertion.
	if got := ds.execTerm("exec_svc1").typed("hi"); got != "" {
		t.Fatalf("wrote %q to a service; a service pane must send nothing", got)
	}
	// And it tells the far end nothing about its size either: there is no
	// terminal there whose size could be wrong.
	if got := ds.execTerm("exec_svc1").size(); got != [2]int{} {
		t.Errorf("resized a service to %v; a read-only pane sends no resize", got)
	}
}

// A plain exec with no TTY is not a session at all — a captured `disco exec`,
// say — and must not become a tab just because services now can.
func TestANonTTYExecThatIsNotAServiceIsNotATab(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.execs = []Exec{{
		ID:        "exec_plain",
		Command:   []string{"git", "status"},
		Tty:       false,
		Live:      true,
		CreatedAt: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	}}
	d, m, _ := openWorkspace(t, ds, "enter")
	// Give the poll a turn to do the wrong thing before concluding it did not.
	d.settle()

	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want none", m.shells.len())
	}
}

// The leader plus S opens what the discobox declares — including the services
// that are not running, which is the half a tab cannot show.
func TestLeaderSOpensTheServicesMenu(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{
		{ID: "discobox-api", Name: "Discobox API", Description: "hot reload", Status: "running"},
		{ID: "otel", Name: "OTEL", Status: "stopped"},
	}
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.wait("the menu", func() bool { return m.dialog != nil })

	frame := plainFrame(m)
	for _, want := range []string{"Services", "Discobox API", "OTEL", "stopped"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the menu should show %q:\n%s", want, frame)
		}
	}
}

// Choosing a service and a verb runs it against that service, and says so.
func TestTheServicesMenuRunsAVerb(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{{ID: "otel", Name: "OTEL", Status: "stopped"}}
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.wait("the menu", func() bool { return m.dialog != nil })
	d.key("1")
	d.wait("the verbs", func() bool { return m.dialog != nil && len(m.dialog.items) == 3 })
	d.key("s")
	d.wait("the verb to run", func() bool { return len(ds.serviceActs) > 0 })

	if got, want := ds.serviceActs[0], "start sbx_one otel"; got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
	d.wait("the report", func() bool { return strings.Contains(m.status, "started OTEL") })
}

// A discobox that declares no services says so rather than opening an empty
// menu: an empty list of things you have never heard of reads as a failure.
func TestTheServicesMenuOnADiscoboxWithNone(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.wait("the report", func() bool { return strings.Contains(m.status, "no services") })

	if m.dialog != nil {
		t.Fatal("a discobox with no services must not open a menu")
	}
}

// A declaration that cannot run is listed with the reason, and cannot be
// chosen: there is nothing to start.
func TestTheServicesMenuShowsUnrunnableDeclarations(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{{ID: "broken", Name: "Broken", Status: "stopped", Problem: "script is not executable"}}
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.wait("the menu", func() bool { return m.dialog != nil })

	if !strings.Contains(plainFrame(m), "not executable") {
		t.Fatalf("the menu should say why it cannot run:\n%s", plainFrame(m))
	}
	if m.dialog.items[0].enabled {
		t.Error("a declaration that cannot run must not be selectable")
	}
}

func TestTheServicesMenuReportsAFailedRead(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.servicesErr = errors.New("sandbox is not up")
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.wait("the report", func() bool { return strings.Contains(m.status, "sandbox is not up") })

	if m.dialog != nil {
		t.Fatal("a failed read must not open a menu")
	}
}
