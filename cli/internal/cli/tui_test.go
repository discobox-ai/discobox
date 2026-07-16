package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

func TestToTUISandboxUsesServerDisplayState(t *testing.T) {
	sandbox := toTUISandbox(apimodel.Sandbox{
		Runtime: apimodel.SandboxRuntime{
			Phase:        "running",
			DesiredState: "stopped",
			DisplayState: apiclientgen.NewOptSandboxRuntimeDisplayState("stopping"),
		},
	})
	if sandbox.State != "stopping" {
		t.Fatalf("state = %q, want stopping", sandbox.State)
	}
}

func TestAPIDataSourceCreateSessionUsesSharedRunCreation(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	commit := strings.TrimSpace(git("rev-parse", "HEAD"))
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/sandboxes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"sbx_9qk5n25t2hh2rv00","projectId":"project-1","createdByUserId":"user-1","config":{"name":"tui-test","image":"","cpuVcpus":0,"memoryBytes":0,"storageBytes":0},"runtime":{"phase":"pending","desiredState":"stopped","lastOperationStatus":"pending","generation":1,"observedGeneration":0,"restartGeneration":0,"restartedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{
		app:       &App{},
		client:    client,
		projectID: "project-1",
	}
	sandbox, err := ds.CreateSession(t.Context(), tui.NewSessionRequest{
		Harness: "codex",
		Path:    repo + "@HEAD",
		Prompt:  "fix the failing tests",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sandbox.ID != "sbx_9qk5n25t2hh2rv00" {
		t.Fatalf("sandbox ID = %q", sandbox.ID)
	}
	if posted["harnessName"] != "codex" {
		t.Fatalf("harnessName = %#v, want codex", posted["harnessName"])
	}
	config := posted["config"].(map[string]any)
	prompt := config["prompt"].([]any)
	if len(prompt) != 1 || prompt[0] != "fix the failing tests" {
		t.Fatalf("prompt = %#v", prompt)
	}
	source := config["source"].(map[string]any)
	checkout := source["checkout"].(map[string]any)
	if checkout["commit"] != commit || checkout["refType"] != "commit" {
		t.Fatalf("checkout = %#v, want HEAD commit %s", checkout, commit)
	}
}

// TestFramedTerminalRead verifies output frames are surfaced as a plain byte
// stream, including a payload split across two Reads.
func TestFramedTerminalRead(t *testing.T) {
	client, server := net.Pipe()
	term := &framedTerminal{frames: &directAttachFrames{conn: client}, events: make(chan tui.TerminalEvent)}
	defer term.Close()

	go func() {
		_, _ = readTerminalFrame(server)
		_ = writeTerminalFrame(server, attachFrameOutput, []byte("abc"))
	}()

	buf := make([]byte, 2)
	n, err := term.Read(buf)
	if err != nil || string(buf[:n]) != "ab" {
		t.Fatalf("first read = %q, %v; want \"ab\", nil", buf[:n], err)
	}
	n, err = term.Read(buf)
	if err != nil || string(buf[:n]) != "c" {
		t.Fatalf("second read = %q, %v; want \"c\", nil", buf[:n], err)
	}
}

// TestFramedTerminalWriteAndResize verifies input and resize both round-trip as
// the expected frame types.
func TestFramedTerminalWriteAndResize(t *testing.T) {
	client, server := net.Pipe()
	term := &framedTerminal{frames: &directAttachFrames{conn: client}, events: make(chan tui.TerminalEvent)}
	defer term.Close()

	go func() {
		_, _ = term.Write([]byte("xy"))
		_ = term.Resize(80, 24)
	}()

	frame, err := readTerminalFrame(server)
	if err != nil || frame.typ != attachFrameInput || string(frame.payload) != "xy" {
		t.Fatalf("input frame = %+v, %v", frame, err)
	}

	frame, err = readTerminalFrame(server)
	if err != nil || frame.typ != attachFrameResize {
		t.Fatalf("resize frame type = %d, %v", frame.typ, err)
	}
	var size struct {
		Cols int `json:"cols"`
		Rows int `json:"rows"`
	}
	if err := json.Unmarshal(frame.payload, &size); err != nil {
		t.Fatalf("resize payload: %v", err)
	}
	if size.Cols != 80 || size.Rows != 24 {
		t.Fatalf("resize = %dx%d, want 80x24", size.Cols, size.Rows)
	}
}

// TestFramedTerminalExit maps a clean exit frame onto io.EOF.
func TestFramedTerminalExit(t *testing.T) {
	client, server := net.Pipe()
	term := &framedTerminal{frames: &directAttachFrames{conn: client}, events: make(chan tui.TerminalEvent)}
	defer term.Close()

	payload, err := json.Marshal(struct {
		Status string `json:"status"`
	}{Status: "success"})
	if err != nil {
		t.Fatalf("marshal exit payload: %v", err)
	}
	go func() {
		_, _ = readTerminalFrame(server)
		_ = writeTerminalFrame(server, attachFrameExit, payload)
	}()

	if _, err := term.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Fatalf("exit read err = %v, want io.EOF", err)
	}
}
