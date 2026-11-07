package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// storedAnswerServer is a server whose discobox is not running: its reads are
// answered with what the discobox last posted, stamped observedAt, and its
// own row says the given runtime state (ADR 26-10-11-657 §4).
func storedAnswerServer(t *testing.T, sandboxID, runtimeState string) *App {
	t.Helper()
	const observedAt = `"observedAt":"2026-10-11T00:00:00Z"`
	const exec = `{"id":"ex_1","status":"running","command":["bash"],"workdir":"/workspace","tty":true,"harnessId":"claude","createdAt":"2026-01-01T00:00:00Z"`
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			t.Errorf("a read of a discobox that is not running sent %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/execs"):
			_, _ = w.Write([]byte(`{"execs":[` + exec + `}],` + observedAt + `}`))
		case strings.HasSuffix(r.URL.Path, "/execs/ex_1"), strings.HasSuffix(r.URL.Path, "/execs/primary"):
			_, _ = w.Write([]byte(exec + `,` + observedAt + `}`))
		case strings.HasSuffix(r.URL.Path, "/services"):
			_, _ = w.Write([]byte(`{"services":[{"id":"web","name":"Web","status":"running"}],` + observedAt + `}`))
		case strings.HasSuffix(r.URL.Path, sandboxID):
			_, _ = w.Write([]byte(runTestSandboxJSON(sandboxID, runtimeState)))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return &App{serverURL: server.URL, autoStart: autoStartServerFalse}
}

// A discobox that stops under an exec leaves a record that says running for
// as long as it stays down. Waiting on it must end, not poll that record.
func TestWaitSandboxExecExitEndsWhenTheDiscoboxStopped(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	app := storedAnswerServer(t, sandboxID, "stopped")
	done := make(chan error, 1)
	go func() {
		_, err := app.waitSandboxExecExit(t.Context(), "project-1", sandboxID, "ex_1")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped before exec ex_1 finished") {
			t.Fatalf("err = %v, want it to say the discobox stopped under the exec", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait is still polling a record the discobox posted before it stopped")
	}
}

// An attach that broke reads the exec to decide whether to reconnect. A
// stored record saying running is not a reason to: reconnecting would start
// the discobox that was just stopped.
func TestSandboxExecAttachDoneDoesNotReconnectOnAStoredRecord(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	t.Run("stopped", func(t *testing.T) {
		app := storedAnswerServer(t, sandboxID, "stopped")
		done, err := app.sandboxExecAttachDone(t.Context(), "project-1", sandboxID, primaryExecID)
		if !done || err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("done = %v, err = %v; want the attach ended, naming the stop", done, err)
		}
	})
	// On its way up: the discobox has not answered yet, so the attach asks
	// again rather than acting on what it posted before.
	t.Run("starting", func(t *testing.T) {
		app := storedAnswerServer(t, sandboxID, "starting")
		done, err := app.sandboxExecAttachDone(t.Context(), "project-1", sandboxID, primaryExecID)
		if done || err != nil {
			t.Fatalf("done = %v, err = %v; want it to retry", done, err)
		}
	})
}

// A listing of a discobox that is not running says so, where a person reads
// it and where a program does.
func TestListingsSayWhenTheyAreNotLive(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	app := storedAnswerServer(t, sandboxID, "stopped")
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	execs, execsAt, err := app.listSandboxExecs(t.Context(), "project-1", sandboxID)
	if err != nil || len(execs) != 1 || execsAt.IsZero() {
		t.Fatalf("execs = %v, observedAt = %v, err = %v", execs, execsAt, err)
	}
	terminals, terminalsAt, err := app.listSandboxTerminals(t.Context(), "project-1", sandboxID)
	if err != nil || len(terminals) != 1 || !terminalsAt.Equal(execsAt) {
		t.Fatalf("terminals = %v, observedAt = %v, err = %v", terminals, terminalsAt, err)
	}
	services, servicesAt, err := listSandboxServices(t.Context(), client, "project-1", sandboxID)
	if err != nil || len(services) != 1 || !servicesAt.Equal(execsAt) {
		t.Fatalf("services = %v, observedAt = %v, err = %v", services, servicesAt, err)
	}

	for _, tc := range []struct {
		list  string
		write func(*App, *cobra.Command, time.Time) error
	}{
		{"execs", func(a *App, cmd *cobra.Command, at time.Time) error { return a.writeSandboxExecs(cmd, execs, at) }},
		{"terminals", func(a *App, cmd *cobra.Command, at time.Time) error {
			return a.writeSandboxTerminals(cmd, terminals, at)
		}},
		{"services", func(a *App, cmd *cobra.Command, at time.Time) error { return a.writeSandboxServices(cmd, services, at) }},
	} {
		t.Run(tc.list, func(t *testing.T) {
			run := func(output string, at time.Time) (string, string) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				if err := tc.write(&App{output: output}, cmd, at); err != nil {
					t.Fatalf("write: %v", err)
				}
				return stdout.String(), stderr.String()
			}
			stdout, stderr := run("", execsAt)
			if !strings.Contains(stderr, "The discobox is not running: this is what it last reported") {
				t.Fatalf("stderr = %q, want it to say the listing is not live", stderr)
			}
			if strings.Contains(stdout, "not running") {
				t.Fatalf("the note is on stdout, where it would be piped on: %q", stdout)
			}
			if _, stderr := run("", time.Time{}); stderr != "" {
				t.Fatalf("a live listing said %q", stderr)
			}

			stdout, _ = run("json", execsAt)
			var body map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &body); err != nil {
				t.Fatalf("decode %q: %v", stdout, err)
			}
			if _, ok := body[tc.list]; !ok {
				t.Fatalf("json has no %s: %s", tc.list, stdout)
			}
			if string(body["observedAt"]) != `"2026-10-11T00:00:00Z"` {
				t.Fatalf("observedAt = %s, want when the discobox posted it", body["observedAt"])
			}
			stdout, _ = run("json", time.Time{})
			if strings.Contains(stdout, "observedAt") {
				t.Fatalf("a live listing carries observedAt: %s", stdout)
			}
		})
	}
}

// The console's workspace opens a tab for every live session a listing
// reports, so it is never handed a listing the discobox posted before it went
// down: attaching to one of those would be what starts it.
func TestConsoleDataSourceReportsAStoredListingAsNotRunning(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	app := storedAnswerServer(t, sandboxID, "stopped")
	source := &apiDataSource{app: app, projectID: "project-1"}
	if execs, err := source.Execs(t.Context(), sandboxID); !errors.Is(err, console.ErrNotRunning) || len(execs) != 0 {
		t.Fatalf("Execs = %v, %v; want tui.ErrNotRunning and no sessions", execs, err)
	}
	if services, err := source.Services(t.Context(), sandboxID); !errors.Is(err, console.ErrNotRunning) || len(services) != 0 {
		t.Fatalf("Services = %v, %v; want tui.ErrNotRunning and no services", services, err)
	}
}

// A command that ran and then lost its discobox has no result on record. The
// record the discobox last posted says running, with no exit code, and must
// not be read as the command having succeeded.
func TestReturnSandboxExecStatusDoesNotReportAStoredRunningRecordAsSuccess(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	app := storedAnswerServer(t, sandboxID, "stopped")
	err := app.returnSandboxExecStatus(t.Context(), "project-1", sandboxID, "ex_1")
	if err == nil || !strings.Contains(err.Error(), "stopped before exec ex_1 reported a result") {
		t.Fatalf("err = %v, want it to say the result is not known", err)
	}
}

// Listing a stopped discobox's tools is refused when it posted none, and the
// refusal is told apart from every other failure, so that running a tool —
// which is use — can start the discobox and ask it.
func TestSandboxToolsMarksARefusedListing(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"discobox sbx_1 is not running and has posted nothing to read"}`))
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = sandboxTools(t.Context(), client, "project-1", "sbx_1")
	if !errors.Is(err, errSandboxToolsNotReadable) || !strings.Contains(err.Error(), "has posted nothing to read") {
		t.Fatalf("err = %v, want errSandboxToolsNotReadable carrying the server's reason", err)
	}
}

// A stopped discobox's tools are what it last posted, and the listing says so
// like every other: the time travels with the catalog.
func TestToolCatalogCarriesWhenAStoredListingWasPosted(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"tools":[{"id":"lint","name":"lint","runs":"sandbox","layer":"source","fileName":"10-lint.sh","script":true,"program":[],"args":[],"env":[]}],"observedAt":"2026-10-11T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	catalog, observedAt, err := app.toolCatalog(t.Context(), client, "project-1", "sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC); !observedAt.Equal(want) {
		t.Fatalf("observedAt = %v, want %v", observedAt, want)
	}
	found := false
	for _, def := range catalog {
		found = found || def.ID == "lint"
	}
	if !found {
		t.Fatalf("catalog = %+v, want the discobox's posted tool in it", catalog)
	}
}
