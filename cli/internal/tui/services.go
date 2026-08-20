package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// The workspace's view of the discobox's declared services (ADR 0063).
//
// A running service is already on screen — it arrives in the exec listing like
// any other session and is drawn as a tab on the right — so what this adds is
// the half a tab cannot show: the services that are *not* running, and the
// three verbs. Both live behind one leader key, because a service is something
// you set going and then stop thinking about, not something you work in.
//
// The menu is opened from the server every time rather than cached. A service
// stopped from another window, or one whose file was added to the repository a
// minute ago, is exactly what it is being opened to find out about.

// Service is one of a discobox's declared services, as the menu draws it.
type Service struct {
	ID          string
	Name        string
	Description string
	// Status is the sandbox's own word for what the service is doing:
	// stopped, starting, running, exited or failed.
	Status string
	// Problem is why a declaration cannot run — a missing shebang or
	// executable bit. Such a service is listed, because the alternative is a
	// file the author believes is a service and that nothing ever mentions.
	Problem string
}

// runnable reports whether this declaration can be acted on at all.
func (s Service) runnable() bool { return strings.TrimSpace(s.Problem) == "" }

// detail is the row's second line: why it cannot run, or what it is for.
func (s Service) detail() string {
	if !s.runnable() {
		return "cannot run: " + s.Problem
	}
	if description := strings.TrimSpace(s.Description); description != "" {
		return s.Status + " · " + description
	}
	return s.Status
}

// displayName is what the service is called on screen.
func (s Service) displayName() string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}
	return s.ID
}

// servicesMsg is the answer to opening the menu: the discobox's services, or
// why they could not be read.
type servicesMsg struct {
	gen      int
	services []Service
	err      error
}

// serviceMenuMsg opens the verb menu for one service. It is a message rather
// than a direct call because a dialog's action returns a command, and the
// command is the only way one dialog can put another in its place.
type serviceMenuMsg struct{ service Service }

// serviceVerbMsg runs one verb against one service.
type serviceVerbMsg struct {
	verb    ServiceVerb
	service Service
}

// openServices asks for the discobox's services and opens the menu on them.
func (m *Model) openServices() tea.Cmd {
	if m.paneBox.ID == "" {
		return nil
	}
	m.busy = "services…"
	gen := m.wsGen
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
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
		return m.report(false, "this discobox declares no services — add one under .discobox/services")
	}
	items := make([]action, 0, len(msg.services))
	for i, service := range msg.services {
		items = append(items, action{
			// The key is the row's number, so the first nine are reachable by
			// digit as well as by moving to them — the same arrangement the
			// harness file picker uses.
			key:     itoa(i + 1),
			label:   service.displayName(),
			detail:  service.detail(),
			enabled: service.runnable(),
			why:     service.Problem,
		})
	}
	services := msg.services
	m.dialog = actionsDialog("Services — "+m.paneBox.Name, "", items, func(key string) tea.Cmd {
		for i, service := range services {
			if itoa(i+1) == key {
				return func() tea.Msg { return serviceMenuMsg{service: service} }
			}
		}
		return nil
	})
	return nil
}

// showServiceMenu is the second half: what to do with the service just chosen.
//
// All three verbs are always offered. The sandbox settles what each one means
// for the state the service is actually in — starting a running service
// changes nothing, stopping a stopped one likewise — and a menu that greys out
// the verb you came for, on a status read a moment ago, is a menu arguing with
// a service that has since moved on.
func (m *Model) showServiceMenu(service Service) tea.Cmd {
	items := []action{
		{key: "s", label: "start", detail: "run it, or leave it running", enabled: true},
		{key: "t", label: "stop", detail: "end its run and keep what it printed", enabled: true},
		{key: "R", label: "restart", detail: "stop it if it is running, then start it", enabled: true},
	}
	m.dialog = actionsDialog(service.displayName(), service.detail(), items, func(key string) tea.Cmd {
		verb := ServiceRestart
		switch key {
		case "s":
			verb = ServiceStart
		case "t":
			verb = ServiceStop
		}
		return func() tea.Msg { return serviceVerbMsg{verb: verb, service: service} }
	})
	return nil
}

// runServiceVerb acts on one service and reports the outcome on the status
// line. What the service does next arrives through the exec listing the
// workspace is already polling, so there is nothing to refresh here: a service
// that starts becomes a tab on its own, and one that stops takes its tab with
// it.
func (m *Model) runServiceVerb(verb ServiceVerb, service Service) tea.Cmd {
	if m.paneBox.ID == "" {
		return nil
	}
	m.busy = string(verb) + "…"
	ctx, ds, id := m.ctx, m.ds, m.paneBox.ID
	name := service.displayName()
	return func() tea.Msg {
		err := ds.DoService(ctx, verb, id, service.ID)
		return serviceDoneMsg{verb: verb, name: name, err: err}
	}
}

// serviceDoneMsg is one finished verb.
type serviceDoneMsg struct {
	verb ServiceVerb
	name string
	err  error
}

func (m *Model) serviceDone(msg serviceDoneMsg) tea.Cmd {
	m.busy = ""
	if msg.err != nil {
		return m.report(true, "cannot %s %s: %v", msg.verb, msg.name, msg.err)
	}
	return m.report(false, "%s", msg.verb.done(msg.name))
}
