package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
)

// desktopAppName is the file name the CLI looks for the app under.
func desktopAppName() string {
	if runtime.GOOS == "windows" {
		return "discobox-desktop.exe"
	}
	return "discobox-desktop"
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The app is found by the flag, then the environment, then beside this
// executable, then on PATH (ADR 26-11-07-983 §3), and a CLI that finds none
// says how to build one.
func TestDesktopBinaryOrder(t *testing.T) {
	onPath := t.TempDir()
	writeExecutable(t, filepath.Join(onPath, desktopAppName()))
	t.Setenv("PATH", onPath)
	t.Setenv(desktopBinaryEnv, "")

	got, err := desktopBinary("")
	if err != nil || got != filepath.Join(onPath, desktopAppName()) {
		t.Fatalf("on PATH: got %q, %v", got, err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(self), desktopAppName())
	writeExecutable(t, beside)
	t.Cleanup(func() { _ = os.Remove(beside) })
	if got, err := desktopBinary(""); err != nil || got != beside {
		t.Fatalf("beside the CLI: got %q, %v, want %q", got, err, beside)
	}

	t.Setenv(desktopBinaryEnv, "/from/env")
	if got, _ := desktopBinary(""); got != "/from/env" {
		t.Fatalf("environment: got %q", got)
	}
	if got, _ := desktopBinary("/from/flag"); got != "/from/flag" {
		t.Fatalf("flag: got %q", got)
	}
}

func TestDesktopBinaryNotFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(desktopBinaryEnv, "")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(self), desktopAppName())); err == nil {
		t.Skip("a desktop app sits beside the test binary")
	}
	_, err = desktopBinary("")
	if err == nil || !strings.Contains(err.Error(), "build:desktop") || !strings.Contains(err.Error(), desktopBinaryEnv) {
		t.Fatalf("got %v, want an error naming build:desktop and %s", err, desktopBinaryEnv)
	}
}

// A workspace opens the primary first, then every other live terminal; the
// console draws services, files-changed sessions and ended execs elsewhere.
func TestDesktopExecsOpensLiveTerminals(t *testing.T) {
	got := desktopExecs([]console.Exec{
		{ID: "p", Primary: true, Live: true, Tty: true},
		{ID: "sh", Command: []string{"/bin/bash", "-l"}, Live: true, Tty: true},
		{ID: "claude", Harness: "claude", Command: []string{"claude"}, Live: true, Tty: true},
		{ID: "diff", Tool: "diff", Live: true, Tty: true},
		{ID: "svc", Service: "web", Live: true, Tty: true},
		{ID: "files", FilesChanged: true, Live: true, Tty: true},
		{ID: "ended", Command: []string{"bash"}, Tty: true},
		{ID: "pipe", Command: []string{"cat"}, Live: true},
	})
	want := []desktopExecInfo{
		{ID: console.ExecPrimary, Kind: "primary", Live: true},
		{ID: "sh", Kind: "shell", Label: "bash", Live: true, Argv: []string{"/bin/bash", "-l"}},
		{ID: "claude", Kind: "terminal", Label: "claude", Live: true, Argv: []string{"claude"}},
		{ID: "diff", Kind: "tool", Label: "diff", Live: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// What the box reports as known wins over the listing's own fields, and a
// harness without a display name is drawn by its slug.
func TestDesktopListingPrefersKnownState(t *testing.T) {
	created := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	got := desktopListing(console.Listing{Sandboxes: []console.Sandbox{{
		ID: "sbx_1", Name: "one", State: "running", Harness: "claude", Branch: "stale", Commit: "old",
		Created: created, CreatedBy: "sbx_0",
		Git:   console.GitState{Known: true, Branch: "main", Commit: "abc1234", Dirty: true},
		Diff:  console.DiffStat{Known: true, Added: 3, Deleted: 1},
		Usage: console.Usage{Known: true, CPUPercent: 12, MemoryBytes: 1 << 20},
	}, {
		ID: "sbx_2", Name: "two", HarnessName: "Codex", Harness: "codex", Branch: "feature",
		Diff: console.DiffStat{Added: 9}, Usage: console.Usage{CPUPercent: 99},
	}}})
	if len(got) != 2 {
		t.Fatalf("got %d rows", len(got))
	}
	one, two := got[0], got[1]
	if one.Harness != "claude" || one.Branch != "main" || one.Commit != "abc1234" || !one.Dirty ||
		one.Added != 3 || one.Deleted != 1 || one.CPU != 12 || one.MemoryBytes != 1<<20 ||
		one.Parent != "sbx_0" || !one.Created.Equal(created) {
		t.Errorf("known state: %+v", one)
	}
	if two.Harness != "Codex" || two.Branch != "feature" || two.Added != 0 || two.CPU != 0 {
		t.Errorf("unknown state: %+v", two)
	}
	if one.ID != "sbx_1" || one.SandboxID != "sbx_1" || one.Parent != "sbx_0" {
		t.Errorf("a primary server's box is keyed by its ID: %+v", one)
	}
	work := desktopListing(console.Listing{Sandboxes: []console.Sandbox{{ID: "sbx_1", Server: "work", CreatedBy: "sbx_0"}}})[0]
	if work.ID != "work/sbx_1" || work.SandboxID != "sbx_1" || work.Server != "work" || work.Parent != "work/sbx_0" {
		t.Errorf("another server's box is keyed by server and ID, its parent too: %+v", work)
	}
}

type bridgeFrame struct {
	kind    byte
	stream  uint32
	payload []byte
}

// readFrames splits what the bridge wrote into its frames.
func readFrames(t *testing.T, data []byte) []bridgeFrame {
	t.Helper()
	var out []bridgeFrame
	r := bytes.NewReader(data)
	for r.Len() > 0 {
		var head [9]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			t.Fatalf("short frame head: %v", err)
		}
		payload := make([]byte, binary.BigEndian.Uint32(head[5:9]))
		if _, err := io.ReadFull(r, payload); err != nil {
			t.Fatalf("short frame payload: %v", err)
		}
		out = append(out, bridgeFrame{head[0], binary.BigEndian.Uint32(head[1:5]), payload})
	}
	return out
}

// lockedBuffer is the bridge's output, read while its goroutines write it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// hello answers with the spec the app draws from, and an op the CLI does not
// know is answered with an error rather than dropped, so the app's request
// never hangs.
func TestDesktopBridgeAnswersRequests(t *testing.T) {
	out := &lockedBuffer{}
	b := &desktopBridge{out: out, leader: "ctrl+a", streams: map[uint32]*desktopStream{}}
	b.handle(context.Background(), bridgeRequest{ID: 1, Op: "hello"})
	b.handle(context.Background(), bridgeRequest{ID: 2, Op: "launchRockets"})

	frames := readFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	var hello struct {
		ID     int64 `json:"id"`
		Result struct {
			Spec json.RawMessage `json:"spec"`
		} `json:"result"`
	}
	if frames[0].kind != bridgeJSON || frames[0].stream != 0 {
		t.Fatalf("hello frame: kind %d stream %d", frames[0].kind, frames[0].stream)
	}
	if err := json.Unmarshal(frames[0].payload, &hello); err != nil || hello.ID != 1 {
		t.Fatalf("hello answer %s: %v", frames[0].payload, err)
	}
	want, err := json.Marshal(tui.Spec("ctrl+a"))
	if err != nil {
		t.Fatal(err)
	}
	var gotSpec, wantSpec any
	if err := json.Unmarshal(hello.Result.Spec, &gotSpec); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantSpec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotSpec, wantSpec) {
		t.Errorf("hello's spec is not tui.Spec(leader)")
	}
	var unknown struct {
		ID    int64  `json:"id"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(frames[1].payload, &unknown); err != nil || unknown.ID != 2 || !strings.Contains(unknown.Error, "launchRockets") {
		t.Fatalf("unknown op answer %s: %v", frames[1].payload, err)
	}
}

// fakeTerminal plays its output once, then ends.
type fakeTerminal struct {
	out    io.Reader
	events chan console.TerminalEvent
	closed chan struct{}
	once   sync.Once
}

func newFakeTerminal(output string) *fakeTerminal {
	return &fakeTerminal{out: strings.NewReader(output), events: make(chan console.TerminalEvent), closed: make(chan struct{})}
}

func (f *fakeTerminal) Read(p []byte) (int, error)           { return f.out.Read(p) }
func (f *fakeTerminal) Write(p []byte) (int, error)          { return len(p), nil }
func (f *fakeTerminal) Resize(int, int) error                { return nil }
func (f *fakeTerminal) Repaint() error                       { return nil }
func (f *fakeTerminal) Events() <-chan console.TerminalEvent { return f.events }
func (f *fakeTerminal) Close() error                         { f.once.Do(func() { close(f.closed) }); return nil }

// An attached terminal's output goes to the app on its stream, and its end is
// one Exit frame, after which the stream is closed and forgotten.
func TestDesktopBridgeStreamsATerminal(t *testing.T) {
	out := &lockedBuffer{}
	b := &desktopBridge{out: out, streams: map[uint32]*desktopStream{}}
	term := newFakeTerminal("hello from the box")
	b.attach(7, term)

	select {
	case <-term.closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the terminal was not closed when it ended")
	}
	b.mu.Lock()
	left := len(b.streams)
	b.mu.Unlock()
	if left != 0 {
		t.Errorf("%d streams left open", left)
	}

	var stdout []byte
	var exits []bridgeFrame
	for _, f := range readFrames(t, out.Bytes()) {
		if f.stream != 7 {
			t.Errorf("frame on stream %d", f.stream)
		}
		switch f.kind {
		case frame.Stdout:
			stdout = append(stdout, f.payload...)
		case frame.Exit:
			exits = append(exits, f)
		}
	}
	if string(stdout) != "hello from the box" {
		t.Errorf("stdout %q", stdout)
	}
	if len(exits) != 1 || len(exits[0].payload) != 0 {
		t.Errorf("exit frames %+v, want one with no error", exits)
	}
}

// A request about a box goes to the server that listed it, never to the
// primary server about another server's box, and a box is known by its server
// and ID together: two servers can list the same ID.
func TestDesktopBridgeRoutesToTheBoxsServer(t *testing.T) {
	primary := &apiDataSource{app: &App{}, projectID: "primary-project"}
	work := &apiDataSource{app: &App{}, projectID: "work-project"}
	ds := &apiDataSource{app: &App{}, projectID: "primary-project", primaryName: "home", servers: []*tuiServer{
		{server: &server{name: "home", primary: true}, source: primary},
		{server: &server{name: "work"}, source: work},
	}}
	b := &desktopBridge{ds: ds, streams: map[uint32]*desktopStream{}, boxes: map[string]desktopBoxRef{}}
	for _, ref := range []desktopBoxRef{{"", "sbx_same"}, {"work", "sbx_same"}, {"gone", "sbx_gone"}} {
		b.boxes[desktopBoxKey(ref.server, ref.id)] = ref
	}
	for key, want := range map[string]string{"sbx_same": "primary-project", "work/sbx_same": "work-project"} {
		got, id, err := b.source(context.Background(), key)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if p := got.(*apiDataSource).projectID; p != want || id != "sbx_same" {
			t.Errorf("%s went to %s as %s, want %s as sbx_same", key, p, id, want)
		}
	}
	if _, _, err := b.source(context.Background(), "gone/sbx_gone"); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("a box on a server that is not answering: got %v", err)
	}
}

// The app gets no discobox credential or server address from this process's
// environment, and keeps everything else, its own variables included.
func TestDesktopEnvCarriesNoDiscoboxCredentials(t *testing.T) {
	got := desktopEnv([]string{
		"PATH=/bin", "DISPLAY=:0", "DISCOBOX_TOKEN=secret", "DISCOBOX_SERVER=https://x",
		"discobox_token=secret", "DISCOBOX_DESKTOP_DEBUG=1", "DISCOBOX_DESKTOP_BINARY=/app",
	})
	want := []string{"PATH=/bin", "DISPLAY=:0", "DISCOBOX_DESKTOP_DEBUG=1", "DISCOBOX_DESKTOP_BINARY=/app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// What the app logs reaches the terminal with its control sequences escaped,
// a line at a time, however the writes are split.
func TestDesktopStderrIsTerminalSafe(t *testing.T) {
	var out bytes.Buffer
	w := &terminalSafeLines{w: &out}
	for _, chunk := range []string{"bridge ← {\"name\":\"a\x1b]52;c;", "aGk=\x07\"}\nnext\u009b", "2J\ttab"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07\u009b") {
		t.Fatalf("a control reached the terminal: %q", got)
	}
	if !strings.Contains(got, "\n") || !strings.Contains(got, "\t") || strings.Count(got, "\n") != 2 {
		t.Errorf("lines and tabs not kept: %q", got)
	}
}

// Ending a stream ends what serves it, though its terminal never closes its
// events channel.
func TestDesktopBridgeClosingAStreamEndsItsGoroutines(t *testing.T) {
	b := &desktopBridge{out: io.Discard, streams: map[uint32]*desktopStream{}}
	term := newFakeTerminal("")
	term.out = untilClosed{term.closed}
	before := runtime.NumGoroutine()
	b.attach(3, term)
	b.closeStream(3)
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, want %d", runtime.NumGoroutine(), before)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// untilClosed is a terminal's output that has nothing until the terminal is
// closed, and then ends.
type untilClosed struct{ closed <-chan struct{} }

func (u untilClosed) Read([]byte) (int, error) {
	<-u.closed
	return 0, io.EOF
}
