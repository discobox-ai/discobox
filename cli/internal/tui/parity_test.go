package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The parity scenarios in the core's testdata/parity are run by both consoles
// (ADR 26-11-07-598 §5): here, pressing each command's binding in this
// window from the registry, and by the desktop app (desktop/) with its own. A
// scenario says what the workspace looks like after each command, in terms
// both can answer, so the two are held to one behavior while each keeps its
// platform's keys.

type parityScenario struct {
	Name     string `json:"name"`
	Attach   bool   `json:"attach"`
	Services []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Logs   string `json:"logs"`
	} `json:"services"`
	Steps []struct {
		Command string `json:"command"`
		Gesture string `json:"gesture"`
		Service string `json:"service"`
		Verb    string `json:"verb"`
		Prompt  string `json:"prompt"`
		Expect  struct {
			Shells    *int    `json:"shells"`
			Terminals *int    `json:"terminals"`
			Box       *int    `json:"box"`
			Focus     *string `json:"focus"`
			Maximized *bool   `json:"maximized"`
			Targets   *int    `json:"targets"`
			Selected  *int    `json:"selected"`
			Archived  *bool   `json:"archived"`
			Services  *int    `json:"services"`
			Output    *string `json:"output"`
			Created   *string `json:"created"`
		} `json:"expect"`
	} `json:"steps"`
}

func TestParityScenarios(t *testing.T) {
	files, err := filepath.Glob("../console/testdata/parity/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no parity scenarios: %v", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var sc parityScenario
		if err := json.Unmarshal(data, &sc); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		t.Run(filepath.Base(file), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ds := newFakeSource(testSandboxes()...)
				ds.serviceLogs = map[string][]byte{}
				for _, v := range sc.Services {
					svc := console.Service{ID: v.ID, Status: v.Status}
					if svc.Live() {
						svc.ExecID = "exec_" + v.ID
					}
					ds.services = append(ds.services, svc)
					ds.serviceLogs[v.ID] = []byte(v.Logs)
				}
				var d *driver
				var m *Model
				if sc.Attach {
					d, m, _ = openWorkspace(t, ds, "enter")
				} else {
					d, m = openList(t, ds)
					m.list.moveTo(0)
				}
				for i, step := range sc.Steps {
					var keys []string
					if step.Service != "" {
						keys = serviceKeys(t, m, ds, step.Service, step.Verb)
					} else if sc.Attach {
						keys = paneKeys(t, m, step.Command)
					} else {
						keys = listKeys(t, step.Command, step.Gesture)
					}
					if step.Prompt != "" {
						// The composer card, the prompt written in it, then
						// Enter for the create card and Enter to create.
						for _, r := range step.Prompt {
							keys = append(keys, string(r))
						}
						keys = append(keys, "enter", "enter")
					}
					for _, k := range keys {
						d.key(k)
					}
					want := step.Expect
					got := func() string {
						focus := "terminals"
						if m.onShells {
							focus = "shells"
						}
						return fmt.Sprintf("box=%d terminals=%d shells=%d services=%d focus=%s maximized=%v targets=%d selected=%d archived=%v created=%q",
							shownRow(m), m.Terminals.Len(), m.Shells.Len(), len(m.ServiceTabs()), focus, m.maximized, len(m.list.targets()), m.list.selectionCount(), m.list.Filter.Archived, created(m, ds))
					}
					d.wait(step.Command+step.Gesture+step.Service+" "+fmt.Sprint(keys)+" from "+got(), func() bool {
						focus := "terminals"
						if m.onShells {
							focus = "shells"
						}
						return (want.Shells == nil || m.Shells.Len() == *want.Shells) &&
							(want.Terminals == nil || m.Terminals.Len() == *want.Terminals) &&
							(want.Box == nil || shownRow(m) == *want.Box) &&
							(want.Focus == nil || focus == *want.Focus) &&
							(want.Maximized == nil || m.maximized == *want.Maximized) &&
							(want.Targets == nil || len(m.list.targets()) == *want.Targets) &&
							(want.Selected == nil || m.list.selectionCount() == *want.Selected) &&
							(want.Archived == nil || m.list.Filter.Archived == *want.Archived) &&
							(want.Services == nil || len(m.ServiceTabs()) == *want.Services) &&
							(want.Output == nil || strings.Contains(plainFrame(m), *want.Output)) &&
							(want.Created == nil || created(m, ds) == *want.Created)
					})
					if t.Failed() {
						t.Fatalf("step %d (%s%s%s, keys %v): %s", i, step.Command, step.Gesture, step.Service, keys, got())
					}
				}
			})
		})
	}
}

// created is the name of the box on screen when it is the one the last
// create made, or nothing.
func created(m *Model, ds *fakeSource) string {
	ds.mu.Lock()
	id := ds.createdID
	runs := len(ds.runs)
	ds.mu.Unlock()
	if runs == 0 || !m.hasWorkspace() || m.Box.ID != id {
		return ""
	}
	return m.Box.Name
}

// shownRow is the row of the list the box whose workspace is on screen is
// on, or -1 with none on screen.
func shownRow(m *Model) int {
	if !m.hasWorkspace() {
		return -1
	}
	return slices.IndexFunc(m.list.Rows(), func(s console.Sandbox) bool { return m.boxRef(s) == m.boxRef(m.Box) })
}

// listKeys is the key that runs a command or a gesture from the list: its
// bare key.
func listKeys(t *testing.T, command, gesture string) []string {
	t.Helper()
	var b console.TUIBinding
	if gesture != "" {
		g, ok := console.LookupGesture(gesture)
		if !ok {
			t.Fatalf("no gesture %s in the registry", gesture)
		}
		b = g.Bindings.TUI
	} else {
		c, ok := console.Lookup(command)
		if !ok {
			t.Fatalf("no command %s in the registry", command)
		}
		b = c.Bindings.TUI
	}
	if len(b.Keys) == 0 {
		t.Fatalf("%s%s has no key in the list", command, gesture)
	}
	return b.Keys[:1]
}

// paneKeys is the key presses that run a command from a workspace's focused
// pane: the leader, then the command's leader sequence.
func paneKeys(t *testing.T, m *Model, id string) []string {
	t.Helper()
	c, ok := console.Lookup(id)
	if !ok {
		t.Fatalf("no command %s in the registry", id)
	}
	if len(c.Bindings.TUI.Leader) == 0 {
		t.Fatalf("command %s has no leader binding to reach it from a pane", id)
	}
	return append([]string{m.leader()}, c.Bindings.TUI.Leader[0]...)
}

// serviceKeys is the key presses that run one of a service's verbs, or show
// its output, from a workspace's focused pane: the services command's, the
// service's number in its menu, and the verb's key in the service's.
func serviceKeys(t *testing.T, m *Model, ds *fakeSource, service, verb string) []string {
	t.Helper()
	ds.mu.Lock()
	i := slices.IndexFunc(ds.services, func(s console.Service) bool { return s.ID == service })
	ds.mu.Unlock()
	if i < 0 {
		t.Fatalf("the box declares no service %s", service)
	}
	key := "v"
	if verb != "output" {
		key = serviceVerbKeys[console.ServiceVerb(verb)]
	}
	if key == "" {
		t.Fatalf("a service has no verb %s", verb)
	}
	return append(paneKeys(t, m, "services"), fmt.Sprint(i+1), key)
}
