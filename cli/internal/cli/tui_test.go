package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net"
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
