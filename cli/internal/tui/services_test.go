package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// runningService is a service the sandbox reports as up, with the exec running
// it — the shape the two listings have together.
func runningService(id, name, execID string) Service {
	return Service{
		ID: id, Name: name, Status: "running", ExecID: execID,
		StartedAt: time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	}
}

// serviceExecRecord is what the exec listing reports for a running service: an
// exec on pipes, tagged with the service it runs.
func serviceExecRecord(id, service, name string) Exec {
	return Exec{
		ID:          id,
		Command:     []string{"/bin/bash", "-lc", "'/src/.discobox/services/10-api.sh'"},
		Service:     service,
		ServiceName: name,
		Tty:         false,
		Live:        true,
		CreatedAt:   time.Date(2026, 8, 7, 12, 30, 0, 0, time.UTC),
	}
}

// A running service is a tab in the left column, after the terminals.
func TestARunningServiceIsATabAfterTheTerminals(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("discobox-api", "Discobox API", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "discobox-api", "Discobox API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want none: a service is not a shell", m.shells.len())
	}
	if !m.terminals.panes[0].primary {
		t.Fatal("the primary must stay the head of the left column")
	}
	p := m.terminals.panes[1]
	if p.service != "discobox-api" {
		t.Errorf("pane service = %q, want discobox-api", p.service)
	}
	if p.action != InteractService {
		t.Errorf("action = %q, want %q", p.action, InteractService)
	}
	if got := p.name(); got != "Discobox API" {
		t.Errorf("tab name = %q, want %q", got, "Discobox API")
	}
	// It is drawing the service's own process, not a card about it.
	if ds.execTerm("exec_svc1") == nil {
		t.Fatal("a running service must be attached to its exec")
	}
}

// The gap this closes: a service that is not running has no exec, so a tab
// strip drawn from the exec listing alone said nothing about it at all — and a
// declaration that cannot run is exactly the one you need to hear about.
func TestABrokenDeclarationGetsATabSayingWhy(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{{
		ID: "discobox-api", Name: "Discobox Api", Status: "stopped",
		FileName: "10-discobox-api.sh",
		Problem:  "front matter: yaml: line 2: mapping values are not allowed in this context",
	}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	p := m.terminals.panes[1]
	if p.service != "discobox-api" {
		t.Fatalf("pane service = %q, want discobox-api", p.service)
	}
	if p.status != "cannot run" {
		t.Errorf("pane status = %q, want %q", p.status, "cannot run")
	}
	// The tab is marked, so the strip says something is wrong without it
	// having to be opened.
	if !strings.Contains(p.name(), "✗") {
		t.Errorf("tab name = %q, want a mark on it", p.name())
	}
	// Only the active pane is drawn, so read the card where it is read: with
	// the tab focused.
	focusService(d, m)
	d.wait("the card", func() bool { return strings.Contains(plainFrame(m), "cannot run") })
	frame := plainFrame(m)
	for _, want := range []string{"Discobox Api", "cannot run", "mapping values", "10-discobox-api.sh"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the pane should say %q:\n%s", want, frame)
		}
	}
}

// A service that failed shows why, and what it printed before it did — which
// after a crash is the reason.
func TestAFailedServiceShowsItsLastOutput(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	code := 1
	ds.services = []Service{{
		ID: "otel", Name: "OTEL", Status: "failed", ExecID: "exec_svc1",
		ExitCode: &code, StartedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}}
	ds.serviceLogs = map[string][]byte{"otel": []byte("docker: permission denied\n")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	focusService(d, m)
	d.wait("the card", func() bool { return strings.Contains(plainFrame(m), "permission denied") })

	frame := plainFrame(m)
	for _, want := range []string{"OTEL", "failed", "exit code 1", "last output", "permission denied"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the pane should say %q:\n%s", want, frame)
		}
	}
	if p := m.terminals.panes[1]; p.status != "failed" {
		t.Errorf("pane status = %q, want failed", p.status)
	}
}

// A service stopped on purpose has no tab: its absence says the right thing,
// and a pane to dismiss every time would be the window nagging.
func TestAStoppedServiceHasNoTab(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{{ID: "otel", Name: "OTEL", Status: "stopped"}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.settle()

	if m.terminals.len() != 1 {
		t.Fatalf("terminals = %d, want just the primary", m.terminals.len())
	}
	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want none", m.shells.len())
	}
}

// Stopping a service takes its tab away, and starting one brings it back: the
// service listing both opens and closes these panes, so there is one writer
// per service rather than a tab nobody owns.
func TestAServicePaneFollowsItsService(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	ds.setServices([]Service{{ID: "otel", Name: "OTEL", Status: "stopped"}})
	d.wait("the tab to go", func() bool { return m.terminals.len() == 1 })

	ds.setServices([]Service{runningService("otel", "OTEL", "exec_svc2")})
	d.wait("the tab to come back", func() bool { return m.terminals.len() == 2 })
}

// A restart keeps the exec id (ADR 0038) and moves the start time, so the pane
// has to notice it is drawing a run that is over and open the new one.
func TestARestartedServiceReopensItsPane(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	first := runningService("otel", "OTEL", "exec_svc1")
	ds.services = []Service{first}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	before := m.terminals.panes[1].id

	restarted := first
	restarted.StartedAt = first.StartedAt.Add(time.Minute)
	ds.setServices([]Service{restarted})
	d.wait("the pane to be reopened", func() bool {
		return m.terminals.len() == 2 && m.terminals.panes[1].id != before
	})
}

// A declaration deleted from the repository takes its tab with it.
func TestADeletedDeclarationLosesItsTab(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{{ID: "otel", Name: "OTEL", Status: "failed"}}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	ds.setServices(nil)
	d.wait("the tab to go", func() bool { return m.terminals.len() == 1 })
}

// The left column is [terminals, services], and services are ordered as the
// repository declares them rather than by when their process started.
func TestServicesSortAfterTerminalsInDeclarationOrder(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	// Declared api first and otel second, started the other way round.
	api := runningService("api", "API", "exec_api")
	api.StartedAt = time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	otel := runningService("otel", "OTEL", "exec_otel")
	otel.StartedAt = time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds.services = []Service{api, otel}
	ds.execs = []Exec{
		serviceExecRecord("exec_api", "api", "API"),
		serviceExecRecord("exec_otel", "otel", "OTEL"),
		{
			ID: "exec_term1", Command: []string{"claude"}, Harness: "claude",
			Tty: true, Live: true, CreatedAt: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		},
	}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("every session", func() bool { return m.terminals.len() == 4 })

	var got []string
	for _, p := range m.terminals.panes {
		got = append(got, p.execID)
	}
	want := []string{ExecPrimary, "exec_term1", servicePaneID("api"), servicePaneID("otel")}
	if !slices.Equal(got, want) {
		t.Fatalf("left column = %v, want %v", got, want)
	}
	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want none", m.shells.len())
	}
}

// Services share the left column, so they never split the window: only shells
// put a second box on screen.
func TestServicesDoNotSplitTheWindow(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	if m.split() {
		t.Fatal("a service must not split the window; it is a tab in the box the primary already has")
	}
	full, rows := m.paneCells(m.width)
	if got := ds.execTerm(ExecPrimary).size(); got != [2]int{full, rows} {
		t.Errorf("primary is %v, want the whole window %dx%d", got, full, rows)
	}
}

// Nothing reads a service's stdin, so nothing types at its pane.
func TestAServicePaneIsReadOnly(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	d.key("ctrl+a")
	d.key(paneRightKey)
	d.wait("focus on the service", func() bool { return m.terminals.active == 1 })
	d.key("h")
	d.key("i")

	if got := ds.execTerm("exec_svc1").typed("hi"); got != "" {
		t.Fatalf("wrote %q to a service; a service pane must send nothing", got)
	}
	if got := ds.execTerm("exec_svc1").size(); got != [2]int{} {
		t.Errorf("resized a service to %v; a read-only pane sends no resize", got)
	}
}

// A plain exec with no TTY is not a session at all — a captured `disco exec` —
// and must not become a tab just because services now can.
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
	d.settle()

	if m.shells.len() != 0 || m.terminals.len() != 1 {
		t.Fatalf("terminals = %d shells = %d, want just the primary", m.terminals.len(), m.shells.len())
	}
}

// The leader plus S opens what the discobox declares, including the services
// that have no tab.
func TestLeaderSOpensTheServicesMenu(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{
		{ID: "discobox-api", Name: "Discobox API", Description: "hot reload", Status: "stopped"},
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
	d.wait("the report", func() bool { return strings.Contains(m.status, "started OTEL") })

	if got, want := ds.acts()[0], "start sbx_one otel"; got != want {
		t.Fatalf("ran %q, want %q", got, want)
	}
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

// A listing that fails is not reported by the poll: a workspace can be opened
// on a sandbox that is still coming up, and a two-second cadence saying so is
// noise the user cannot act on.
func TestAFailedServicePollIsNotReported(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.servicesErr = errors.New("sandbox is not up")
	d, m, _ := openWorkspace(t, ds, "enter")
	d.settle()

	if strings.Contains(m.status, "sandbox is not up") {
		t.Fatalf("the poll reported a failed listing: %q", m.status)
	}
}

// focusService moves the keys onto the service tab beside the primary, which
// is the only way to read what it is drawing: a column draws its active pane.
func focusService(d *driver, m *Model) {
	d.key("ctrl+a")
	d.key(paneRightKey)
	d.wait("focus on the service", func() bool { return m.terminals.active == 1 })
}
