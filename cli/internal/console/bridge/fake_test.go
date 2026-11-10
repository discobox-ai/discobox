package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// fakeCore is a console.DataSource for one server, and the window's root
// over every server. A call the test did not prepare panics through the nil
// embedded interface.
type fakeCore struct {
	console.DataSource

	mu      sync.Mutex
	name    string
	servers map[string]*fakeCore
	listing console.Listing
	execs   []console.Exec
	calls   []string

	// do answers a verb; nil answers with no note.
	do func(ctx context.Context, verb console.Verb, id string, n *console.Narration) (string, error)
	// opened is every terminal OpenExec, NewShell and NewTerminal made, by
	// exec ID; gate, when set, holds OpenExec until it is closed.
	opened map[string]*fakeTerm
	gate   chan struct{}
	// settings is what BoxSettings reads, and update what UpdateBox was sent.
	settings   console.BoxSettings
	update     *console.BoxSettingsUpdate
	requests   []console.CredentialRequest
	rejections []console.SecretRejection
	// stopped answers Execs as a box that is not running.
	stopped bool
	// echo has every terminal echo what it is sent as its output (fakeTerm).
	echo bool
	// view is the list's filter as the last window left it: what Session
	// hands back and SaveView records.
	view console.ListView
	// lists is how many times the listing was read.
	lists int
}

func newFakeCore(name string, boxes ...console.Sandbox) *fakeCore {
	f := &fakeCore{name: name, listing: console.Listing{Sandboxes: boxes}, opened: map[string]*fakeTerm{}}
	f.servers = map[string]*fakeCore{"": f}
	return f
}

func (f *fakeCore) call(format string, args ...any) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
	f.mu.Unlock()
}

func (f *fakeCore) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeCore) PrimaryServer() string { return f.name }

func (f *fakeCore) ForServer(server string) console.DataSource {
	f.mu.Lock()
	s := f.servers[server]
	f.mu.Unlock()
	if s != nil {
		return s
	}
	return nil
}

func (f *fakeCore) Session(context.Context) (console.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return console.Session{Project: "proj", Directory: "/src", Branch: "main", View: f.view}, nil
}

func (f *fakeCore) SaveView(_ context.Context, view console.ListView) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.view = view
	return nil
}

func (f *fakeCore) List(context.Context) (console.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return f.listing, nil
}

func (f *fakeCore) setListing(boxes ...console.Sandbox) {
	f.mu.Lock()
	f.listing = console.Listing{Sandboxes: boxes}
	f.mu.Unlock()
}

func (f *fakeCore) Do(ctx context.Context, verb console.Verb, id string, n *console.Narration) (string, error) {
	f.call("do %s %s", verb, id)
	if f.do != nil {
		return f.do(ctx, verb, id, n)
	}
	return "", nil
}

func (f *fakeCore) Rename(_ context.Context, id, name string) error {
	f.call("rename %s %q", id, name)
	return nil
}

func (f *fakeCore) BoxSettings(context.Context, string) (console.BoxSettings, error) {
	return f.settings, nil
}

func (f *fakeCore) Harnesses(context.Context, string) ([]console.Harness, error) { return nil, nil }

func (f *fakeCore) UpdateBox(_ context.Context, id string, u console.BoxSettingsUpdate) error {
	f.call("update %s", id)
	f.mu.Lock()
	f.update = &u
	f.mu.Unlock()
	return nil
}

func (f *fakeCore) Execs(context.Context, string) ([]console.Exec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return nil, console.ErrNotRunning
	}
	return f.execs, nil
}

func (f *fakeCore) Services(context.Context, string) ([]console.Service, error) { return nil, nil }

func (f *fakeCore) term(id string) *fakeTerm {
	t := newFakeTerm()
	f.mu.Lock()
	t.echo = f.echo
	f.opened[id] = t
	f.mu.Unlock()
	return t
}

func (f *fakeCore) opening(id string) *fakeTerm {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened[id]
}

func (f *fakeCore) OpenExec(ctx context.Context, box, exec string, cols, rows int) (console.Terminal, error) {
	f.call("open %s %s %dx%d", box, exec, cols, rows)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if exec == "missing" {
		return nil, errors.New("no such session")
	}
	return f.term(exec), nil
}

func (f *fakeCore) NewShell(_ context.Context, box string, cols, rows int) (console.Exec, console.Terminal, error) {
	f.call("shell %s %dx%d", box, cols, rows)
	ex := console.Exec{ID: "sh1", Command: []string{"/bin/bash"}, Live: true, Tty: true}
	return ex, f.term(ex.ID), nil
}

func (f *fakeCore) NewTerminal(_ context.Context, box string, cols, rows int) (console.Exec, console.Terminal, error) {
	f.call("terminal %s %dx%d", box, cols, rows)
	ex := console.Exec{ID: "t1", Harness: "claude", Command: []string{"claude"}, Live: true, Tty: true}
	return ex, f.term(ex.ID), nil
}

func (f *fakeCore) EndExec(_ context.Context, box, exec string) error {
	f.call("end %s %s", box, exec)
	if exec == "stuck" {
		return errors.New("the session would not end")
	}
	return nil
}

func (f *fakeCore) PutImage(_ context.Context, box, name string, data []byte) (string, error) {
	f.call("put %s %s %d", box, name, len(data))
	return "/tmp/" + name, nil
}

func (f *fakeCore) CredentialRequests(context.Context) ([]console.CredentialRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests, nil
}

func (f *fakeCore) SecretRejections(context.Context) ([]console.SecretRejection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejections, nil
}

func (f *fakeCore) Resources(context.Context) (console.Resources, error) {
	return console.Resources{Known: true, CPUVCPUs: 1, CPUCapacity: 8}, nil
}

// fakeTerm is a box's terminal: what the app wrote to it, the sizes it was
// given, and output a test sends. One that echoes sends each of those back
// as a line of its output, for an app that cannot see into it, and ends on
// the input "exit\r".
type fakeTerm struct {
	mu      sync.Mutex
	echo    bool
	ended   bool
	got     []string
	out     chan []byte
	events  chan console.TerminalEvent
	closed  chan struct{}
	once    sync.Once
	pending []byte
}

func newFakeTerm() *fakeTerm {
	return &fakeTerm{out: make(chan []byte, 16), events: make(chan console.TerminalEvent, 1), closed: make(chan struct{})}
}

func (t *fakeTerm) note(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.got = append(t.got, s)
	if !t.echo || t.ended {
		return
	}
	t.out <- []byte(s + "\n")
	if s == "input exit\r" {
		t.ended = true
		close(t.out)
	}
}

func (t *fakeTerm) log() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.got...)
}

func (t *fakeTerm) Read(p []byte) (int, error) {
	if len(t.pending) == 0 {
		select {
		case b, ok := <-t.out:
			if !ok {
				return 0, io.EOF
			}
			t.pending = b
		case <-t.closed:
			return 0, io.ErrClosedPipe
		}
	}
	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	return n, nil
}

func (t *fakeTerm) Write(p []byte) (int, error) {
	t.note("input " + string(p))
	return len(p), nil
}

func (t *fakeTerm) Resize(cols, rows int) error {
	t.note(fmt.Sprintf("resize %dx%d", cols, rows))
	return nil
}

func (t *fakeTerm) Repaint() error {
	t.note("repaint")
	return nil
}

func (t *fakeTerm) Events() <-chan console.TerminalEvent { return t.events }

func (t *fakeTerm) Close() error {
	t.once.Do(func() { close(t.closed) })
	return nil
}

func (t *fakeTerm) isClosed() bool {
	select {
	case <-t.closed:
		return true
	default:
		return false
	}
}

// app is the app's end of a bridge to a Server over pipes.
type app struct {
	t      *testing.T
	out    io.WriteCloser
	frames chan Frame
	nextID uint64
	// seen is every JSON message read, in order, for asserting on what came
	// between.
	seen []Message
	done chan error
	// offSchema is set by a test while it sends what the wire's schema does
	// not describe on purpose, and cleared after.
	offSchema bool
}

func dial(t *testing.T, ds console.DataSource) *app {
	t.Helper()
	appIn, srvOut := io.Pipe()
	srvIn, appOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	a := &app{t: t, out: appOut, frames: make(chan Frame, 256), done: make(chan error, 1)}
	go func() { a.done <- NewServer(ds, srvOut).Serve(ctx, srvIn) }()
	go func() {
		defer close(a.frames)
		for {
			f, err := ReadFrame(appIn)
			if err != nil {
				return
			}
			a.frames <- f
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = appOut.Close()
		_ = srvOut.Close()
	})
	return a
}

// hello opens the bridge, and is the welcome.
func (a *app) hello() Welcome {
	a.send(Message{Type: TypeHello, Hello: &Hello{Protocol: Protocol}})
	return *a.expect(TypeWelcome).Welcome
}

func (a *app) send(m Message) {
	a.t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		a.t.Fatal(err)
	}
	a.conforms(wireMessage, data)
	a.frame(KindJSON, 0, data)
}

func (a *app) frame(kind byte, stream uint32, payload []byte) {
	a.t.Helper()
	if err := NewWriter(a.out).Write(kind, stream, payload); err != nil {
		a.t.Fatal(err)
	}
}

func (a *app) resize(stream uint32, cols, rows int) {
	data, err := json.Marshal(frame.ResizePayload{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		a.t.Fatal(err)
	}
	a.conforms(wireResize, data)
	a.frame(frame.Resize, stream, data)
}

// command sends a command and is its ID.
func (a *app) command(c Command) uint64 {
	a.nextID++
	c.ID = a.nextID
	if c.Targets == nil {
		c.Targets = []string{}
	}
	a.send(Message{Type: TypeCommand, Command: &c})
	return c.ID
}

// next is the next frame, failing the test after a while without one.
func (a *app) next() Frame {
	a.t.Helper()
	select {
	case f, ok := <-a.frames:
		if !ok {
			a.t.Fatal("the bridge closed")
		}
		return f
	case <-time.After(10 * time.Second):
		a.t.Fatal("nothing arrived")
	}
	return Frame{}
}

// message is the next JSON message, skipping stream frames.
func (a *app) message() Message {
	a.t.Helper()
	for {
		f := a.next()
		if f.Kind != KindJSON {
			continue
		}
		a.conforms(wireMessage, f.Payload)
		var m Message
		if err := json.Unmarshal(f.Payload, &m); err != nil {
			a.t.Fatalf("%s: %v", f.Payload, err)
		}
		a.seen = append(a.seen, m)
		return m
	}
}

// expect is the next message of a type, skipping others.
func (a *app) expect(t Type) Message {
	a.t.Helper()
	for {
		if m := a.message(); m.Type == t {
			return m
		}
	}
}

// result is the result of one command, skipping anything else.
func (a *app) result(id uint64) Result {
	a.t.Helper()
	for {
		m := a.expect(TypeResult)
		if m.Result.ID == id {
			return *m.Result
		}
	}
}

// quiet fails if a JSON message arrives within a short while.
func (a *app) quiet(within time.Duration) {
	a.t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f := <-a.frames:
			if f.Kind == KindJSON {
				a.t.Fatalf("unexpected message %s", f.Payload)
			}
		case <-deadline:
			return
		}
	}
}

func running(id string) console.Sandbox {
	return console.Sandbox{ID: id, Name: id, State: console.StateRunning, HasRuntime: true}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The wire's schema (wire.schema.json, generated from wire.go), which the
// desktop app's types are generated from. Every message the fake app sends
// or reads is held to it, so a message the Go writes that the schema does
// not describe — a null where a list belongs — fails here rather than in the
// app.
var wireCompiler = sync.OnceValues(func() (*jsonschema.Compiler, error) {
	data, err := os.ReadFile("wire.schema.json")
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	return c, c.AddResource("wire.schema.json", doc)
})

var (
	wireMessage = compileWire("wire.schema.json")
	wireResize  = compileWire("wire.schema.json#/$defs/ResizePayload")
)

func compileWire(ref string) func() (*jsonschema.Schema, error) {
	return sync.OnceValues(func() (*jsonschema.Schema, error) {
		c, err := wireCompiler()
		if err != nil {
			return nil, err
		}
		return c.Compile(ref)
	})
}

// conforms fails the test when payload is not what the schema describes.
func (a *app) conforms(schema func() (*jsonschema.Schema, error), payload []byte) {
	t := a.t
	t.Helper()
	if a.offSchema {
		return
	}
	sch, err := schema()
	if err != nil {
		t.Fatalf("the wire's schema: %v", err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("%s: %v", payload, err)
	}
	if err := sch.Validate(v); err != nil {
		t.Errorf("%s is not on the wire's schema: %v", payload, err)
	}
}
