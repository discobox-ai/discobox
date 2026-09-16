package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

// transferSandboxJSON is one discobox as the API reports it, with the observed power
// state the caller wants to control.
func transferSandboxJSON(id, name, runtimeState string) string {
	runtime := `"state":"ready","desiredState":"present","generation":1,"observedGeneration":1`
	if runtimeState != "" {
		runtime += `,"runtimeState":"` + runtimeState + `"`
	}
	return `{"id":"` + id + `","projectId":"project-1","createdByUserId":"user-1","displayName":"` + name + `",` +
		`"config":{"name":"` + name + `","image":""},"runtime":{` + runtime + `},` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
}

// A running discobox is refused rather than copied while it writes: a tar of a
// live tree can catch a git index part way through, and the reader finds out
// only after the transfer (ADR 0123 §2).
func TestExportRefusesARunningDiscobox(t *testing.T) {
	var exported bool
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/export") {
			exported = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(transferSandboxJSON("sbx_1", "my-box", "running")))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse, projectID: "project-1"}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.prepareSandboxForExport(context.Background(), client, "project-1", "sbx_1", false, io.Discard)
	if err == nil {
		t.Fatal("a running discobox was exported")
	}
	if !strings.Contains(err.Error(), "--stop") {
		t.Errorf("err = %q; it should say how to get past the refusal", err)
	}
	if exported {
		t.Error("the export was started before the refusal")
	}
}

// --stop stops it and leaves it stopped: an export is usually the first half of
// a move, and starting the source back up to archive it a moment later is work
// nobody wanted.
func TestExportWithStopStopsAndWaits(t *testing.T) {
	var stopped bool
	var started bool
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/stop"):
			stopped = true
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/start"):
			started = true
			w.WriteHeader(http.StatusAccepted)
		}
		state := "running"
		if stopped {
			state = "stopped"
		}
		_, _ = w.Write([]byte(transferSandboxJSON("sbx_1", "my-box", state)))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse, projectID: "project-1"}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := app.prepareSandboxForExport(context.Background(), client, "project-1", "sbx_1", true, io.Discard)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !stopped {
		t.Error("the discobox was not stopped")
	}
	if started {
		t.Error("the discobox was started again; --stop leaves it stopped")
	}
	if sandboxIsRunning(sandbox) {
		t.Error("the wait returned while the discobox was still running")
	}
}

func TestExportWritesTheArchiveToAFile(t *testing.T) {
	archive := []byte("pretend this is a tar")
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/export") {
			w.Header().Set("Content-Type", exportMediaType)
			_, _ = w.Write(archive)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(transferSandboxJSON("sbx_1", "my-box", "stopped")))
	}))
	defer server.Close()

	destination := filepath.Join(t.TempDir(), "my-box.dbox")
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse, projectID: "project-1"}
	var stderr bytes.Buffer
	if err := app.exportSandboxTo(context.Background(), "project-1", "sbx_1", destination, io.Discard, &stderr); err != nil {
		t.Fatalf("export: %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archive) {
		t.Fatalf("file = %q, want the archive verbatim", got)
	}

	// A second run of the obvious command must not silently overwrite the first
	// one's archive.
	err = app.exportSandboxTo(context.Background(), "project-1", "sbx_1", destination, io.Discard, &stderr)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want a refusal to overwrite", err)
	}
}

func TestExportDefaultFileNameUsesTheDiscoboxName(t *testing.T) {
	sandbox := decodeSandbox(t, transferSandboxJSON("sbx_1", "my-box", ""))
	if got := exportDefaultFileName(sandbox); got != "my-box.dbox" {
		t.Errorf("got %q, want my-box.dbox", got)
	}
	// A name is not a path: whatever it holds, the export lands beside the
	// working directory rather than somewhere a separator pointed it.
	slashed := decodeSandbox(t, transferSandboxJSON("sbx_2", "team/box", ""))
	if got := exportDefaultFileName(slashed); strings.ContainsAny(got, `/\`) {
		t.Errorf("got %q, which is not one path element", got)
	}
}

func TestImportPostsTheArchiveAndReportsWarnings(t *testing.T) {
	var gotBody []byte
	var gotQuery, gotContentType string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sandbox":` + transferSandboxJSON("sbx_new", "my-box", "") +
			`,"warnings":["OPENAI_API_KEY was bound to a secret named \"openai\", which this project does not have"]}`))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse, projectID: "project-1"}
	result, err := app.importSandbox(context.Background(), "project-1",
		strings.NewReader("pretend this is a tar"),
		sandboxImportOptions{name: "my-box-2", poolID: "pool_9", harnessSlug: "codex"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if string(gotBody) != "pretend this is a tar" {
		t.Errorf("body = %q", gotBody)
	}
	if gotContentType != exportMediaType {
		t.Errorf("content type = %q, want %q", gotContentType, exportMediaType)
	}
	for _, want := range []string{"name=my-box-2", "pool=pool_9", "harness=codex"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query = %q, want it to carry %q", gotQuery, want)
		}
	}
	if result.Sandbox == nil || result.Sandbox.ID != "sbx_new" {
		t.Fatalf("sandbox = %+v", result.Sandbox)
	}
	var stderr bytes.Buffer
	reportImportWarnings(&stderr, result)
	if !strings.Contains(stderr.String(), "openai") {
		t.Errorf("stderr = %q, want the unmatched secret named", stderr.String())
	}
}

func TestImportSurfacesTheServersRefusal(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"this project has no harness claude (Claude Code); configure it here first"}`))
	}))
	defer server.Close()

	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse, projectID: "project-1"}
	_, err := app.importSandbox(context.Background(), "project-1", strings.NewReader("x"), sandboxImportOptions{})
	if err == nil {
		t.Fatal("import succeeded against a 409")
	}
	if !strings.Contains(err.Error(), "Claude Code") {
		t.Fatalf("err = %q, want the server's reason", err)
	}
}

// A transfer streams one server's export into the other's import, stops the
// source first, and archives it once the destination confirms.
func TestTransferStreamsBetweenServersAndArchivesTheSource(t *testing.T) {
	archive := []byte("pretend this is a whole workspace")
	var sourceStopped, sourceDeleted bool
	source := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/export") {
			w.Header().Set("Content-Type", exportMediaType)
			_, _ = w.Write(archive)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/stop"):
			sourceStopped = true
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete:
			sourceDeleted = true
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/sandboxes"):
			_, _ = w.Write([]byte(`{"sandboxes":[` + transferSandboxJSON("sbx_1", "my-box", "running") + `]}`))
			return
		}
		state := "running"
		if sourceStopped {
			state = "stopped"
		}
		_, _ = w.Write([]byte(transferSandboxJSON("sbx_1", "my-box", state)))
	}))
	defer source.Close()

	var received []byte
	destination := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sandboxes/import") {
			received, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandbox":` + transferSandboxJSON("sbx_moved", "my-box", "") + `}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[]}`))
	}))
	defer destination.Close()

	app := &App{serverURL: source.URL, autoStart: autoStartServerFalse, projectID: "project-1", output: "table"}
	target := &server{name: "lab", address: destination.URL, app: app.forServer(destination.URL)}
	cmd := newTestCommand(t)
	if err := app.transferSandbox(cmd, "sbx_1", target, sandboxImportOptions{}, false); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if !sourceStopped {
		t.Error("the source was not stopped before it was read")
	}
	if !bytes.Equal(received, archive) {
		t.Errorf("the destination received %q, want the export verbatim", received)
	}
	if !sourceDeleted {
		t.Error("the source was not archived; a move moves")
	}
}

// --keep makes the same command a copy.
func TestTransferKeepLeavesTheSourceInPlace(t *testing.T) {
	var sourceDeleted bool
	source := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/export") {
			w.Header().Set("Content-Type", exportMediaType)
			_, _ = w.Write([]byte("archive"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			sourceDeleted = true
			w.WriteHeader(http.StatusAccepted)
		} else if strings.HasSuffix(r.URL.Path, "/sandboxes") {
			_, _ = w.Write([]byte(`{"sandboxes":[` + transferSandboxJSON("sbx_1", "my-box", "stopped") + `]}`))
			return
		}
		_, _ = w.Write([]byte(transferSandboxJSON("sbx_1", "my-box", "stopped")))
	}))
	defer source.Close()
	destination := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sandboxes/import") {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"sandbox":` + transferSandboxJSON("sbx_copy", "my-box", "") + `}`))
			return
		}
		_, _ = w.Write([]byte(`{"projects":[]}`))
	}))
	defer destination.Close()

	app := &App{serverURL: source.URL, autoStart: autoStartServerFalse, projectID: "project-1", output: "table"}
	target := &server{name: "lab", address: destination.URL, app: app.forServer(destination.URL)}
	if err := app.transferSandbox(newTestCommand(t), "sbx_1", target, sandboxImportOptions{}, true); err != nil {
		t.Fatalf("transfer --keep: %v", err)
	}
	if sourceDeleted {
		t.Error("--keep archived the source anyway")
	}
}

func TestTransferTargetRefusesAnUnknownServer(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	app := &App{serverURL: "http://127.0.0.1:1", autoStart: autoStartServerFalse}
	if _, err := app.transferTarget("nowhere"); err == nil {
		t.Fatal("an unregistered name was accepted")
	}
	// An address always has a scheme and a registered name never can, so which
	// one was written is never a guess.
	target, err := app.transferTarget("https://lab.example.com")
	if err != nil {
		t.Fatalf("an address was refused: %v", err)
	}
	if target.address != "https://lab.example.com" {
		t.Errorf("address = %q", target.address)
	}
}

// newTestCommand is a cobra command with its streams discarded, for the code
// paths that write a discobox to the command's output.
func newTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetContext(t.Context())
	return cmd
}

func decodeSandbox(t *testing.T, body string) *apimodel.Sandbox {
	t.Helper()
	var sandbox apimodel.Sandbox
	if err := json.Unmarshal([]byte(body), &sandbox); err != nil {
		t.Fatal(err)
	}
	return &sandbox
}

// A transfer puts two servers on screen at once, and the two lines that say
// what became of somebody's data name them. addressLabel drops the port —
// it also feeds registered-name derivation, where a colon is invalid — so two
// servers on one host rendered identically, which is exactly the pair a
// transfer is most likely to be pointed at by mistake.
func TestEndpointLabelTellsTwoServersOnOneHostApart(t *testing.T) {
	source := endpointLabel("http://127.0.0.1:34101")
	target := endpointLabel("http://127.0.0.1:34102")
	if source == target {
		t.Fatalf("both servers render as %q; a transfer's progress lines cannot tell them apart", source)
	}
	if source != "127.0.0.1:34101" {
		t.Errorf("got %q, want the port kept", source)
	}
	// A name is still a name: nothing else about labeling changes.
	if got := endpointLabel("unix:///tmp/discobox/server.sock"); got != "local" {
		t.Errorf("unix socket label = %q, want local", got)
	}
	if got := endpointLabel("https://lab.example.com"); got != "lab.example.com" {
		t.Errorf("label with no port = %q, want the bare host", got)
	}
	// And addressLabel is left alone, because a registered name may hold no
	// colon (validServerName).
	if got := addressLabel("http://127.0.0.1:34101"); got != "127.0.0.1" {
		t.Errorf("addressLabel = %q; it still has to yield something nameable", got)
	}
}
