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

// A running service is a tab in the left column, ahead of the terminals.
func TestARunningServiceIsATabBeforeTheTerminals(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("discobox-api", "Discobox API", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "discobox-api", "Discobox API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	if m.shells.len() != 0 {
		t.Fatalf("shells = %d, want none: a service is not a shell", m.shells.len())
	}
	if m.primary() == nil || m.terminals.panes[1] != m.primary() {
		t.Fatal("the primary must stay the head of the terminals, behind the services")
	}
	p := m.terminals.panes[0]
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

	p := m.terminals.panes[0]
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
	if p := m.terminals.panes[0]; p.status != "failed" {
		t.Errorf("pane status = %q, want failed", p.status)
	}
}

// A plain exec has no screen to repaint from, so attaching to a running
// service starts at "now" and the pane would sit empty until it next said
// something. Its transcript is played in first.
func TestARunningServicePaneOpensOnItsHistory(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	ds.serviceLogs = map[string][]byte{"otel": []byte("Dashboard is running\r\n")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	focusService(d, m)
	d.wait("the history", func() bool { return strings.Contains(plainFrame(m), "Dashboard is running") })

	// And the live stream carries on from there, into the same pane.
	ds.execTerm("exec_svc1").send("request served\r\n")
	d.wait("the live output", func() bool { return strings.Contains(plainFrame(m), "request served") })
	if !strings.Contains(plainFrame(m), "Dashboard is running") {
		t.Fatalf("the history was lost when the live stream started:\n%s", plainFrame(m))
	}
}

// A transcript longer than a pane could usefully hold is cut, at a line
// boundary so the cut never lands inside an escape sequence.
func TestHistoryIsTailedAtALineBoundary(t *testing.T) {
	line := strings.Repeat("x", 99) + "\n"
	logs := []byte(strings.Repeat(line, (historyLimit/len(line))+50))
	got := tailHistory(logs)
	if len(got) > historyLimit {
		t.Fatalf("history is %d bytes, want at most %d", len(got), historyLimit)
	}
	if !strings.HasPrefix(string(got), "x") {
		t.Fatalf("history starts mid-line: %q", string(got[:20]))
	}
	if short := []byte("short\n"); len(tailHistory(short)) != len(short) {
		t.Fatal("a transcript under the limit must be played whole")
	}
}

// A service never takes the keys. Nobody asked for it — it appeared because
// the discobox is running it — and it is read-only, so focus there is focus
// nowhere.
func TestAServiceDoesNotTakeTheFocus(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	d.settle()

	if m.terminals.visible() != m.primary() {
		t.Fatalf("active = %d, want the primary", m.terminals.active)
	}
	if m.onShells {
		t.Fatal("the keys must stay in the terminals column")
	}
}

// The primary usually arrives last — it waits on its harness install, while a
// service is already running — so the tab that got there first must not be
// left holding the keys.
func TestThePrimaryTakesTheFocusWhenItArrivesAfterAService(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	// Start over with the arrivals under the test's control: a service first,
	// the primary after it.
	m.closeWorkspace()
	gen := m.wsGen
	d.dispatch(serviceTermMsg{
		gen:     gen,
		service: runningService("otel", "OTEL", "exec_svc1"),
		term:    newFakeTerminal(),
	})
	if m.terminals.len() != 1 {
		t.Fatalf("terminals = %d, want the service", m.terminals.len())
	}
	d.dispatch(workspaceTermMsg{gen: gen, exec: Exec{ID: ExecPrimary, Primary: true}, term: newFakeTerminal()})

	if m.terminals.len() != 2 {
		t.Fatalf("terminals = %d, want both", m.terminals.len())
	}
	if m.primary() == nil || m.terminals.visible() != m.primary() {
		t.Fatalf("active = %d (%q), want the primary",
			m.terminals.active, m.terminals.panes[m.terminals.active].execID)
	}
}

// A shell asked for by hand keeps the keys even when the primary lands after
// it: the primary claims the index in its own column, not the window's focus.
func TestAnAskedForShellKeepsTheFocusWhenThePrimaryArrives(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")

	m.closeWorkspace()
	gen := m.wsGen
	d.dispatch(workspaceTermMsg{
		gen:   gen,
		exec:  Exec{ID: "exec_shell1", Command: []string{"/bin/zsh"}, Tty: true, Live: true},
		term:  newFakeTerminal(),
		asked: InteractShell,
		focus: true,
	})
	d.dispatch(workspaceTermMsg{gen: gen, exec: Exec{ID: ExecPrimary, Primary: true}, term: newFakeTerminal()})

	if !m.onShells {
		t.Fatal("the shell that was asked for must keep the keys")
	}
}

// A service arriving beside a terminal you are working in must not move the
// keys off it, even though it lands ahead of it in the strip and shifts every
// index along.
func TestAnArrivingServiceLeavesTheWorkingPaneAlone(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(paneTerminalKey)
	d.wait("the terminal", func() bool { return m.terminals.len() == 2 })
	if m.terminals.active != 1 {
		t.Fatalf("active = %d, want the terminal just opened", m.terminals.active)
	}
	working := m.terminals.panes[1]

	ds.setServices([]Service{{ID: "otel", Name: "OTEL", Status: "failed"}})
	d.wait("the service tab", func() bool { return m.terminals.len() == 3 })

	if m.terminals.visible() != working {
		t.Fatalf("the keys moved to %q; they belong to the pane being worked in",
			m.terminals.panes[m.terminals.active].execID)
	}
}

// On a service, the list's own stop and start keys are the service's: the pane
// you are looking at is what a verb applies to.
func TestStopAndStartOnAServicePaneActOnTheService(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"t", "stop sbx_one otel"},
		{"T", "start sbx_one otel"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			ds := newFakeSource(testSandboxes()...)
			ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
			ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
			d, m, _ := openWorkspace(t, ds, "enter")
			d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
			focusService(d, m)

			d.key("ctrl+a")
			d.key(tc.key)
			d.wait("the verb to run", func() bool { return len(ds.acts()) > 0 })

			if got := ds.acts()[0]; got != tc.want {
				t.Fatalf("ran %q, want %q", got, tc.want)
			}
			// And the discobox itself was left alone.
			if len(ds.verbs()) != 0 {
				t.Fatalf("also ran %v against the discobox", ds.verbs())
			}
		})
	}
}

// Everywhere else those keys still mean the discobox, including on the pane
// right next to the service.
func TestStopOnATerminalPaneStillActsOnTheDiscobox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	// The primary has the keys, as it does by default.
	d.key("ctrl+a")
	d.key("t")
	d.wait("the verb to run", func() bool { return len(ds.verbs()) > 0 })

	if got := ds.verbs()[0]; got != "stop sbx_one" {
		t.Fatalf("ran %q, want %q", got, "stop sbx_one")
	}
	if len(ds.acts()) != 0 {
		t.Fatalf("also ran %v against a service", ds.acts())
	}
}

// A verb with no meaning for a service is not re-scoped: it still applies to
// the discobox, which is the only thing it could apply to.
func TestAnUnrelatedVerbOnAServicePaneActsOnTheDiscobox(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	focusService(d, m)

	d.key("ctrl+a")
	d.key("x")
	d.wait("the verb to run", func() bool { return len(ds.verbs()) > 0 })

	if got := ds.verbs()[0]; got != "archive sbx_one" {
		t.Fatalf("ran %q, want %q", got, "archive sbx_one")
	}
}

// The hints line under a focused service says what can be done to it, and does
// not promise it keys: nothing at the far end reads them.
func TestTheHintsLineOnAServiceOffersItsVerbs(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	// On the primary it is the discobox's line, unchanged.
	if got := hintLine(m.hints()); !strings.Contains(got, "every key goes to the box") {
		t.Fatalf("terminal hints = %q, want the box's line", got)
	}

	focusService(d, m)
	got := hintLine(m.hints())
	for _, want := range []string{"read-only", "t stop", "T start", "S0 services"} {
		if !strings.Contains(got, want) {
			t.Errorf("service hints = %q, want it to offer %q", got, want)
		}
	}
	if strings.Contains(got, "every key goes to") {
		t.Errorf("service hints = %q, but nothing reads a service's input", got)
	}
	if strings.Contains(got, "s shell") {
		t.Errorf("service hints = %q, want the row spent on the service's own verbs", got)
	}
}

// A service is the one pane you deliberately look away from, so its tab is
// where it says something happened while you were not looking.
func TestAServiceTabMarksOutputYouHaveNotSeen(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	// The primary has the screen, so the service is the one not being read.
	ds.execTerm("exec_svc1").send("a request arrived\r\n")
	d.wait("the mark", func() bool { return m.terminals.panes[0].unread })

	if !strings.Contains(plainFrame(m), "•") {
		t.Fatalf("the strip should mark the service:\n%s", plainFrame(m))
	}

	// Looking at it is what clears the mark.
	focusService(d, m)
	d.wait("the mark to clear", func() bool { return !m.terminals.panes[0].unread })
	if strings.Contains(plainFrame(m), "•") {
		t.Fatalf("the mark outlived being read:\n%s", plainFrame(m))
	}
}

// Output that arrives while the service is on screen is output you are
// reading, and is never marked.
func TestOutputOnAVisibleServiceIsNotMarked(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("otel", "OTEL", "exec_svc1")}
	ds.execs = []Exec{serviceExecRecord("exec_svc1", "otel", "OTEL")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	focusService(d, m)

	ds.execTerm("exec_svc1").send("a request arrived\r\n")
	d.wait("the output", func() bool { return strings.Contains(plainFrame(m), "a request arrived") })

	if m.terminals.panes[0].unread {
		t.Fatal("output on the pane being read was marked unread")
	}
}

// Only services are marked. A shell running a build would wear the mark
// permanently while saying nothing you did not already know.
func TestAShellIsNotMarked(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })
	// Back to the primary, so the shell is the one not being read.
	d.key("ctrl+a")
	d.key(paneLeftKey)
	d.wait("focus on the terminals", func() bool { return !m.onShells })

	ds.execTerm("exec_shell1").send("building…\r\n")
	d.settle()

	if m.shells.panes[0].unread {
		t.Fatal("a shell was marked unread")
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
	before := m.terminals.panes[0].id

	restarted := first
	restarted.StartedAt = first.StartedAt.Add(time.Minute)
	ds.setServices([]Service{restarted})
	d.wait("the pane to be reopened", func() bool {
		return m.terminals.len() == 2 && m.terminals.panes[0].id != before
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

// The left column is [services, terminals], and services are ordered as the
// repository declares them rather than by when their process started.
func TestServicesSortBeforeTerminalsInDeclarationOrder(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	// Declared otel first and api second — the other way round from both
	// their start times and their ids, so neither can pass for declaration
	// order by accident.
	otel := runningService("otel", "OTEL", "exec_otel")
	otel.StartedAt = time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	api := runningService("api", "API", "exec_api")
	api.StartedAt = time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	ds.services = []Service{otel, api}
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
	want := []string{servicePaneID("otel"), servicePaneID("api"), ExecPrimary, "exec_term1"}
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

// The leader plus S0 opens what the discobox declares, including the services
// that have no tab.
func TestLeaderS0OpensTheServicesMenu(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{
		{ID: "discobox-api", Name: "Discobox API", Description: "hot reload", Status: "stopped"},
		{ID: "otel", Name: "OTEL", Status: "stopped"},
	}
	d, m, _ := openWorkspace(t, ds, "enter")

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.key(paneServicesMenuKey)
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
	d.key(paneServicesMenuKey)
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
	d.key(paneServicesMenuKey)
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
	d.key(paneServicesMenuKey)
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
	d.key(paneServicesMenuKey)
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

// paneWhich names a pane for a failure message, and says so when there is none
// rather than reading as a pane called nothing.
func paneWhich(p *pane) string {
	if p == nil {
		return "no pane"
	}
	return p.execID
}

// focusService moves the keys onto the service tab beside the primary, which
// is the only way to read what it is drawing: a column draws its active pane.
func focusService(d *driver, m *Model) {
	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.key("1")
	d.wait("focus on the service", func() bool {
		p := m.column().visible()
		return p != nil && p.service != ""
	})
}

// The services have an alphabet of their own one keystroke further in: the
// leader, S, and the number the tab wears.
func TestTheSChordJumpsToAServiceByItsNumber(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{
		runningService("api", "API", "exec_api"),
		runningService("otel", "OTEL", "exec_otel"),
	}
	ds.execs = []Exec{
		serviceExecRecord("exec_api", "api", "API"),
		serviceExecRecord("exec_otel", "otel", "OTEL"),
	}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("both service tabs", func() bool { return m.terminals.len() == 3 })

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.key("2")
	if got := m.column().visible(); got == nil || got.service != "otel" {
		t.Fatalf("S2 landed on %s, want otel", paneWhich(got))
	}

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.key("1")
	if got := m.column().visible(); got == nil || got.service != "api" {
		t.Fatalf("S1 landed on %s, want api", paneWhich(got))
	}
}

// A number with no service under it is answered rather than swallowed.
func TestAnUnknownServiceNumberIsReported(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("api", "API", "exec_api")}
	ds.execs = []Exec{serviceExecRecord("exec_api", "api", "API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	d.key("ctrl+a")
	d.key(paneServicesKey)
	d.key("4")
	d.wait("the report", func() bool { return strings.Contains(m.status, "no service S4") })
}

// The two countings are separate: a service wears S1 and the terminals and
// shells keep the digits to themselves.
func TestServiceTabsWearSNumbersAndLeaveTheDigitsAlone(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("api", "API", "exec_api")}
	ds.execs = []Exec{serviceExecRecord("exec_api", "api", "API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })

	frame := plainFrame(m)
	for _, want := range []string{"[ S1 API ]", "[ 0 attach ]", "[ 1 zsh ]"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the strip should wear %q:\n%s", want, frame)
		}
	}
}

// A service starting is not something you did, so it must not move a shell out
// from under the digit you were reaching for.
func TestAServiceStartingDoesNotRenumberAShell(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key("s")
	d.wait("the shell", func() bool { return m.shells.len() == 1 })
	shell := m.shells.panes[0]

	ds.setServices([]Service{runningService("api", "API", "exec_api")})
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })

	d.key("ctrl+a")
	d.key("1")
	if got := m.column().visible(); got != shell {
		t.Fatalf("1 landed on %s, want the shell it landed on before the service started", paneWhich(got))
	}
}

// The primary is 0 and `a` goes back to it, past the services that sit ahead
// of it in the strip.
func TestLeaderAGoesBackToThePrimaryPastTheServices(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	ds.services = []Service{runningService("api", "API", "exec_api")}
	ds.execs = []Exec{serviceExecRecord("exec_api", "api", "API")}
	d, m, _ := openWorkspace(t, ds, "enter")
	d.wait("the service tab", func() bool { return m.terminals.len() == 2 })
	focusService(d, m)

	d.key("ctrl+a")
	d.key("a")
	if got := m.column().visible(); got != m.primary() {
		t.Fatalf("a landed on %s, want the primary", paneWhich(got))
	}

	focusService(d, m)
	d.key("ctrl+a")
	d.key("0")
	if got := m.column().visible(); got != m.primary() {
		t.Fatalf("0 landed on %s, want the primary", paneWhich(got))
	}
}
