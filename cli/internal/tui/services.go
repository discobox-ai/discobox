package tui

import (
	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The workspace's view of the discobox's declared services (ADR 0070).
//
// Service panes are kept in the terminals column without bringing back the
// chrome in a minimal attach. The services menu offers their output and the
// three lifecycle verbs, including declarations that have never run.
//
// The menu is opened from the server every time rather than cached. A service
// stopped from another window, or one whose file was added to the repository a
// minute ago, is exactly what it is being opened to find out about.

// servicesMsg is the answer to opening the menu: the discobox's services, or
// why they could not be read.
type servicesMsg struct {
	gen      int
	services []console.Service
	err      error
}

// serviceMenuMsg opens the verb menu for one service. It is a message rather
// than a direct call because a dialog's action returns a command, and the
// command is the only way one dialog can put another in its place.
type serviceMenuMsg struct{ service console.Service }

// serviceOutputMsg focuses the service pane after its menu has closed.
type serviceOutputMsg struct{ id string }

// serviceVerbMsg runs one verb against one service.
type serviceVerbMsg struct {
	verb    console.ServiceVerb
	service console.Service
}

// openServices asks for the discobox's services and opens the menu on them.
func (m *Model) openServices() tea.Cmd {
	m.commands.Use("services")
	if m.Box.ID == "" {
		return nil
	}
	m.busy = "services…"
	gen := m.wsGen
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		services, err := ds.Services(ctx, id)
		return servicesMsg{gen: gen, services: services, err: err}
	}
}

// showServices puts the service list on screen. A discobox that declares none
// says so rather than opening an empty menu: an empty list of things you have
// never heard of reads as a failure.
func (m *Model) showServices(msg servicesMsg) tea.Cmd {
	if msg.gen != m.wsGen {
		return nil
	}
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "services: %v", msg.err)
	}
	if len(msg.services) == 0 {
		return m.report(false, "%s", console.NoServices)
	}
	items := make([]action, 0, len(msg.services))
	for i, service := range msg.services {
		n := itoa(i + 1)
		items = append(items, action{
			// The key is the row's number, so the first nine are reachable by
			// digit as well as by moving to them — the same arrangement the
			// harness file picker uses.
			key:     n,
			press:   n,
			label:   service.DisplayName(),
			detail:  service.Detail(),
			enabled: true,
		})
	}
	services := msg.services
	m.dialog = actionsDialog("Services — "+m.Box.Name, "", items, func(key string) tea.Cmd {
		for i, service := range services {
			if itoa(i+1) == key {
				return func() tea.Msg { return serviceMenuMsg{service: service} }
			}
		}
		return nil
	})
	return nil
}

// showServiceMenu is the second half: what to do with the service just chosen
// — its output, then the core's verbs for it (console.Service.Actions).
func (m *Model) showServiceMenu(service console.Service) tea.Cmd {
	items := []action{
		{key: "v", press: "v", label: "view output", detail: "show the service log", enabled: m.Terminals.ByExec(console.ServiceTabID(service.ID)) != nil, why: "no output pane is available"},
	}
	for _, a := range service.Actions() {
		k := serviceVerbKeys[a.Verb]
		items = append(items, action{key: k, press: k, label: a.Label, detail: a.Detail, enabled: a.Enabled, why: a.Why})
	}
	m.dialog = actionsDialog(service.DisplayName(), service.Detail(), items, func(key string) tea.Cmd {
		if key == "v" {
			return func() tea.Msg { return serviceOutputMsg{id: service.ID} }
		}
		for verb, k := range serviceVerbKeys {
			if k == key {
				return func() tea.Msg { return serviceVerbMsg{verb: verb, service: service} }
			}
		}
		return nil
	})
	return nil
}

// serviceVerbKeys is the key each of a service's verbs takes in its menu.
var serviceVerbKeys = map[console.ServiceVerb]string{
	console.ServiceStart:   "s",
	console.ServiceStop:    "t",
	console.ServiceRestart: "R",
}

// runServiceVerb acts on one service, as the core runs it
// (console.RunServiceVerb), and reports the outcome on the status line. What
// the service does next arrives through the listings the workspace is
// already polling, so there is nothing to refresh here: a service that
// starts becomes a tab on its own, and one that stops takes its tab with it.
func (m *Model) runServiceVerb(verb console.ServiceVerb, service console.Service) tea.Cmd {
	if m.Box.ID == "" {
		return nil
	}
	m.busy = string(verb) + "…"
	ctx, ds, id := m.ctx, m.ds.ForServer(m.Box.Server), m.Box.ID
	return func() tea.Msg {
		note, err := console.RunServiceVerb(ctx, ds, id, service.ID, verb)
		return serviceDoneMsg{note: note, err: err}
	}
}

// serviceDoneMsg is one finished verb: what to say of it, or why it failed.
type serviceDoneMsg struct {
	note string
	err  error
}

func (m *Model) serviceDone(msg serviceDoneMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "%v", msg.err)
	}
	return m.report(false, "%s", msg.note)
}
