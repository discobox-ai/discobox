package cli

import (
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// hostile is text a box can put in a service's declaration, or in the account
// its sandbox gives of the service: an OSC that retitles the user's window, a
// CSI that clears their screen, and a bidirectional override.
const hostile = "web\x1b]0;pwned\x07\x1b[2J\u202e"

// hostileShown is how hostile reads once it is terminal-safe.
const hostileShown = `web\x1b]0;pwned\a\x1b[2J\u202e`

// A service's text is the box's, and the console draws it outside every pane's
// emulator — the tab strip, a pane's title edge and footer, the services menu,
// the status line — so it reaches the console escaped. Its IDs are what the
// console sends back, and are left alone.
func TestTUIServiceArrivesTerminalSafe(t *testing.T) {
	t.Parallel()
	got := tuiService(apimodel.SandboxService{
		ID:          "web",
		Name:        hostile,
		Description: apiclientgen.NewOptString(hostile),
		Status:      apiclientgen.SandboxServiceStatus(hostile),
		Problem:     apiclientgen.NewOptString(hostile),
		FileName:    apiclientgen.NewOptString(hostile),
		ExecId:      apiclientgen.NewOptString("exec_1"),
		Error:       apiclientgen.NewOptString(hostile),
	})
	for field, value := range map[string]string{
		"name":        got.Name,
		"description": got.Description,
		"status":      got.Status,
		"problem":     got.Problem,
		"file name":   got.FileName,
		"error":       got.Error,
	} {
		if value != hostileShown {
			t.Errorf("%s = %q, want %q", field, value, hostileShown)
		}
	}
	if got.ID != "web" || got.ExecID != "exec_1" {
		t.Errorf("ids = %q, %q; they are sent back and must be left as they are", got.ID, got.ExecID)
	}

	unnamed := tuiService(apimodel.SandboxService{ID: hostile, Status: apiclientgen.SandboxServiceStatusStopped})
	if unnamed.Name != hostileShown || unnamed.ID != hostile {
		t.Errorf("unnamed: name %q, id %q; want the escaped id shown and the raw one kept", unnamed.Name, unnamed.ID)
	}

	plain := tuiService(apimodel.SandboxService{ID: "api", Name: `Discobox "API" — dev`, Status: apiclientgen.SandboxServiceStatusRunning})
	if plain.Name != `Discobox "API" — dev` || plain.Status != "running" {
		t.Errorf("printable text changed: name %q, status %q", plain.Name, plain.Status)
	}
}

// A service's exec carries the service's name in its metadata, and the
// workspace makes it the tab's title before the services listing arrives.
func TestTUIExecServiceNameArrivesTerminalSafe(t *testing.T) {
	t.Parallel()
	got := tuiExec(apimodel.SandboxExec{
		ID: "exec_1",
		Metadata: apiclientgen.OptSandboxExecMetadata{Set: true, Value: apiclientgen.SandboxExecMetadata{
			sandboxServiceIDMetadata:   "web",
			sandboxServiceNameMetadata: hostile,
		}},
	})
	if got.ServiceName != hostileShown {
		t.Errorf("service name = %q, want %q", got.ServiceName, hostileShown)
	}
	if got.Service != "web" {
		t.Errorf("service id = %q, want it as sent", got.Service)
	}

	unnamed := tuiExec(apimodel.SandboxExec{
		ID: "exec_2",
		Metadata: apiclientgen.OptSandboxExecMetadata{Set: true, Value: apiclientgen.SandboxExecMetadata{
			sandboxServiceIDMetadata: hostile,
		}},
	})
	if unnamed.ServiceName != hostileShown || unnamed.Service != hostile {
		t.Errorf("unnamed: name %q, id %q; want the escaped id shown and the raw one kept", unnamed.ServiceName, unnamed.Service)
	}

	shell := tuiExec(apimodel.SandboxExec{ID: "exec_3"})
	if shell.ServiceName != "" {
		t.Errorf("an exec that is no service has service name %q", shell.ServiceName)
	}
}
