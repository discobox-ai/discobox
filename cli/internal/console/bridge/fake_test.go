package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	// addressed is the listed servers' names by how a box link may name
	// them (AddressedServer).
	addressed map[string]string
	// forward is what Forward hands out, and forwardErr why it cannot.
	forward    *fakeForward
	forwardErr error
	// primaryGone fails an open of the primary.
	primaryGone bool
	// services is what every box declares, and serviceLogs each one's last
	// output, by service ID.
	services    []console.Service
	serviceLogs map[string]string
	// harnesses is what Harnesses lists, harnessSecrets what answers each
	// one's variables, and changes what a box's harness changes are. An
	// edit writes the file out holding "before", has it edited, and reads it
	// back.
	harnesses      []console.Harness
	harnessSecrets []console.HarnessSecret
	changes        console.HarnessChanges
	// workspace is what Workspace reads of every source; run answers Run,
	// and made is the box a Run with none makes; drafts is every prompt
	// SaveDraft was sent.
	workspace console.SourceWorkspace
	run       func(ctx context.Context, req console.RunRequest, n *console.Narration) (console.Sandbox, error)
	made      console.Sandbox
	ran       *console.RunRequest
	drafts    []string
	// tools is what every box declares, and addresses what each answers
	// on; hostErr fails a tool run on this machine. A tool file's edit
	// writes it out under the file's extension, holding its default, has it
	// edited where it is, and reads it back: under toolDir when the test
	// set one, to read what is there afterwards.
	tools     []console.Tool
	addresses console.Addresses
	hostErr   error
	toolDir   string
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

func (f *fakeCore) AddressedServer(_ context.Context, server string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.addressed[server]
	return name, ok, nil
}

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

func (f *fakeCore) Harnesses(_ context.Context, server string) ([]console.Harness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "harnesses@"+server)
	return slices.Clone(f.harnesses), nil
}

func (f *fakeCore) HarnessSecrets(_ context.Context, _, id string) ([]console.HarnessSecret, error) {
	f.call("secrets %s", id)
	return f.harnessSecrets, nil
}

func (f *fakeCore) Pools(context.Context, string) ([]console.Pool, error) { return nil, nil }

func (f *fakeCore) GitCredentialPolicy(context.Context) (string, error) { return "", nil }

func (f *fakeCore) ResolveSource(_ context.Context, source string) (console.Source, error) {
	if source == "nowhere" {
		return console.Source{}, errors.New("nowhere is not a directory")
	}
	return console.Source{Value: "/abs/" + source}, nil
}

func (f *fakeCore) SaveLastSource(_ context.Context, source string) error {
	f.call("last source %s", source)
	return nil
}

func (f *fakeCore) SaveDraft(_ context.Context, prompt string) error {
	f.mu.Lock()
	f.drafts = append(f.drafts, prompt)
	f.mu.Unlock()
	return nil
}

func (f *fakeCore) DoHarness(_ context.Context, _ string, verb console.HarnessVerb, id string) error {
	f.call("harness %s %s", verb, id)
	if verb == console.HarnessSetDefault {
		f.mu.Lock()
		for i := range f.harnesses {
			f.harnesses[i].Default = f.harnesses[i].ID == id
		}
		f.mu.Unlock()
	}
	return nil
}

func (f *fakeCore) CreateHarnessVariant(_ context.Context, _ string, v console.NewHarnessVariant) (console.Harness, error) {
	f.call("variant %s %s %s", v.ParentID, v.Name, v.Slug)
	h := console.Harness{ID: "hc_" + v.Slug, Name: v.Name, Slug: v.Slug, Parent: v.ParentID}
	f.mu.Lock()
	f.harnesses = append(f.harnesses, h)
	f.mu.Unlock()
	return h, nil
}

func (f *fakeCore) EditHarnessFile(ctx context.Context, _ string, id, path string, n *console.Narration) (bool, error) {
	file, err := os.CreateTemp("", "fake-edit-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString("before"); err != nil {
		return false, err
	}
	file.Close()
	if err := n.Edit(ctx, file.Name(), "Editing "+path); err != nil {
		f.call("edit %s %s: %v", id, path, err)
		return false, err
	}
	after, err := os.ReadFile(file.Name())
	changed := string(after) != "before"
	f.call("edit %s %s changed=%v", id, path, changed)
	return changed, err
}

func (f *fakeCore) Tools(context.Context, string) ([]console.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tools, nil
}

func (f *fakeCore) Addresses(context.Context, string) (console.Addresses, error) {
	return f.addresses, nil
}

func (f *fakeCore) RunHostTool(_ context.Context, box, tool string) error {
	f.call("host %s %s", box, tool)
	return f.hostErr
}

func (f *fakeCore) NewTool(_ context.Context, box, tool string, cols, rows int) (console.Exec, console.Terminal, error) {
	f.call("tool %s %s %dx%d", box, tool, cols, rows)
	ex := console.Exec{ID: "tool-" + tool, Tool: tool, Command: []string{tool}, Live: true, Tty: true}
	return ex, f.term(ex.ID), nil
}

func (f *fakeCore) ToolFilePath(file console.ToolFile) string {
	return "/config/discobox/tools/" + file.Tool + "/" + file.Name
}

func (f *fakeCore) EditToolFile(ctx context.Context, tf console.ToolFile, n *console.Narration) (bool, error) {
	dir := f.toolDir
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "fake-tools-"); err != nil {
			return false, err
		}
		defer os.RemoveAll(dir)
	}
	// The copy itself, edited in place, as the CLI's is: made from the
	// default the first time.
	path := filepath.Join(dir, tf.Name)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(path, []byte(tf.Default), 0o600); err != nil {
			return false, err
		}
	}
	if err := n.Edit(ctx, path, "Editing "+tf.Name); err != nil {
		f.call("edit tool %s %s: %v", tf.Tool, tf.Name, err)
		return false, err
	}
	after, err := os.ReadFile(path)
	changed := string(after) != tf.Default
	f.call("edit tool %s %s changed=%v", tf.Tool, tf.Name, changed)
	return changed, err
}

func (f *fakeCore) HarnessChanges(_ context.Context, id string) (console.HarnessChanges, error) {
	f.call("changes %s", id)
	return f.changes, nil
}

func (f *fakeCore) SaveHarnessChanges(_ context.Context, id, candidate string) (string, error) {
	f.call("save changes %s %s", id, candidate)
	return "saved", nil
}

func (f *fakeCore) DiscardHarnessChanges(_ context.Context, id, candidate string) (string, error) {
	f.call("discard changes %s %s", id, candidate)
	return "discarded", nil
}

func (f *fakeCore) Workspace(context.Context, string) (console.SourceWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workspace, nil
}

// Run makes the box made (or what run says), says it was taken, and lists
// it from then on.
func (f *fakeCore) Run(ctx context.Context, req console.RunRequest, n *console.Narration) (console.Sandbox, error) {
	f.mu.Lock()
	f.ran = &req
	run, made := f.run, f.made
	f.mu.Unlock()
	if run != nil {
		return run(ctx, req, n)
	}
	n.Report("pushing the source")
	n.Accept(ctx, made)
	f.mu.Lock()
	f.listing.Sandboxes = append(f.listing.Sandboxes, made)
	f.mu.Unlock()
	return made, nil
}

func (f *fakeCore) ranWith() *console.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ran
}

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

func (f *fakeCore) Services(context.Context, string) ([]console.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return nil, console.ErrNotRunning
	}
	return slices.Clone(f.services), nil
}

func (f *fakeCore) ServiceLogs(_ context.Context, _, service string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.serviceLogs[service]), nil
}

func (f *fakeCore) DoService(_ context.Context, verb console.ServiceVerb, box, service string) error {
	f.call("%s %s %s", verb, box, service)
	return nil
}

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
	if exec == "missing" || (exec == console.ExecPrimary && f.primaryGone) {
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

func (f *fakeCore) Forward(_ context.Context, box string) (console.Forward, error) {
	f.call("forward %s", box)
	if f.forwardErr != nil {
		return nil, f.forwardErr
	}
	return f.forward, nil
}

// fakeForward is a box's ports forwarded: bind moves what it has bound and
// wakes the follower.
type fakeForward struct {
	mu       sync.Mutex
	bindings []console.Binding
	events   chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func newFakeForward(bindings ...console.Binding) *fakeForward {
	return &fakeForward{bindings: bindings, events: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (w *fakeForward) bind(bindings ...console.Binding) {
	w.mu.Lock()
	w.bindings = bindings
	w.mu.Unlock()
	w.events <- struct{}{}
}

func (w *fakeForward) Bindings() []console.Binding {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]console.Binding(nil), w.bindings...)
}

func (w *fakeForward) Events() <-chan struct{} { return w.events }

func (w *fakeForward) Close() error {
	w.once.Do(func() { close(w.closed) })
	return nil
}

func (w *fakeForward) isClosed() bool {
	select {
	case <-w.closed:
		return true
	default:
		return false
	}
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
