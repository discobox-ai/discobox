package tui

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// fakeSource is an in-memory DataSource for driving the screens in tests.
type fakeSource struct {
	mu        sync.Mutex
	sandboxes []Sandbox
	deleted   []string
	deleteErr map[string]error
	terminal  *fakeTerminal
	openErr   error
	attached  []string
	attachErr error

	harnesses   []Harness
	paths       []string
	defaultPath string
	created     []NewSessionRequest
	createErr   error
	createOut   Sandbox

	configs      []HarnessConfig
	definitions  []HarnessDefinition
	saved        []SaveHarnessRequest
	saveErr      error
	saveOut      HarnessConfig
	deletedCfgs  []string
	deleteCfgErr error
	setDefaults  []string
	configured   []string
	configureErr error
}

func (f *fakeSource) ListSandboxes(context.Context) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Sandbox, len(f.sandboxes))
	copy(out, f.sandboxes)
	return out, nil
}

func (f *fakeSource) DeleteSandbox(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	if f.deleteErr != nil {
		return f.deleteErr[id]
	}
	return nil
}

func (f *fakeSource) OpenTerminal(context.Context, string, int, int) (Terminal, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	if f.terminal == nil {
		f.terminal = newFakeTerminal()
	}
	return f.terminal, nil
}

func (f *fakeSource) AttachTerminal(_ context.Context, id string, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attached = append(f.attached, id)
	return f.attachErr
}

func (f *fakeSource) attachedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.attached...)
}

func (f *fakeSource) deletedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *fakeSource) ListHarnesses(context.Context) ([]Harness, error) {
	return append([]Harness(nil), f.harnesses...), nil
}

func (f *fakeSource) PathOptions(context.Context) ([]string, error) {
	return append([]string(nil), f.paths...), nil
}

func (f *fakeSource) DefaultPath() string {
	if f.defaultPath != "" {
		return f.defaultPath
	}
	return "/work"
}

func (f *fakeSource) CreateSession(_ context.Context, req NewSessionRequest) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	if f.createErr != nil {
		return Sandbox{}, f.createErr
	}
	return f.createOut, nil
}

func (f *fakeSource) createdReqs() []NewSessionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]NewSessionRequest(nil), f.created...)
}

func (f *fakeSource) ListHarnessConfigs(context.Context) ([]HarnessConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HarnessConfig(nil), f.configs...), nil
}

func (f *fakeSource) ListHarnessDefinitions(context.Context) ([]HarnessDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HarnessDefinition(nil), f.definitions...), nil
}

func (f *fakeSource) SaveHarness(_ context.Context, req SaveHarnessRequest) (HarnessConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, req)
	if f.saveErr != nil {
		return HarnessConfig{}, f.saveErr
	}
	return f.saveOut, nil
}

func (f *fakeSource) DeleteHarness(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedCfgs = append(f.deletedCfgs, id)
	return f.deleteCfgErr
}

func (f *fakeSource) SetDefaultHarness(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setDefaults = append(f.setDefaults, id)
	return nil
}

func (f *fakeSource) savedReqs() []SaveHarnessRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SaveHarnessRequest(nil), f.saved...)
}

func (f *fakeSource) ConfigureHarness(_ context.Context, id string, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured = append(f.configured, id)
	return f.configureErr
}

func (f *fakeSource) configuredIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.configured...)
}

// fakeTerminal is a scriptable Terminal backed by an in-memory pipe.
type fakeTerminal struct {
	out     *io.PipeReader
	outW    *io.PipeWriter
	writes  chan []byte
	resizes chan [2]int
	closed  chan struct{}
	events  chan TerminalEvent
	once    sync.Once
}

func newFakeTerminal() *fakeTerminal {
	pr, pw := io.Pipe()
	return &fakeTerminal{
		out:     pr,
		outW:    pw,
		writes:  make(chan []byte, 16),
		resizes: make(chan [2]int, 16),
		closed:  make(chan struct{}),
		events:  make(chan TerminalEvent, 8),
	}
}

func (t *fakeTerminal) Read(p []byte) (int, error) { return t.out.Read(p) }

func (t *fakeTerminal) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	select {
	case t.writes <- cp:
	case <-t.closed:
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

func (t *fakeTerminal) Resize(cols, rows int) error {
	select {
	case t.resizes <- [2]int{cols, rows}:
	case <-t.closed:
	}
	return nil
}

func (t *fakeTerminal) Close() error {
	t.once.Do(func() { close(t.closed) })
	return t.outW.Close()
}

func (t *fakeTerminal) Events() <-chan TerminalEvent { return t.events }

// feed pushes output bytes to the terminal's reader.
func (t *fakeTerminal) feed(data string) { _, _ = t.outW.Write([]byte(data)) }

// makeSandboxes builds n sandboxes with stable ids/timestamps for tests.
func makeSandboxes(n int) []Sandbox {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Sandbox, n)
	for i := range out {
		out[i] = Sandbox{
			ID:      idFor(i),
			Name:    nameFor(i),
			State:   "running",
			Created: base.Add(time.Duration(i) * time.Minute),
			Updated: base.Add(time.Duration(i) * time.Minute),
		}
	}
	return out
}

func idFor(i int) string   { return "sb_" + string(rune('a'+i)) }
func nameFor(i int) string { return "sandbox-" + string(rune('a'+i)) }

// newTestScreen builds a sandbox screen sized and loaded with the given list.
func newTestScreen(f *fakeSource) *sandboxesScreen {
	s := newSandboxesScreen(context.Background(), f, defaultKeyMap(), defaultStyles())
	s.setSize(120, 20)
	s.applySandboxes(f.sandboxes)
	return s
}

// keyPress builds a KeyPressMsg from a short spec ("j", "space", "enter", "up").
func keyPress(spec string) tea.KeyPressMsg {
	switch spec {
	case "space":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeySpace})
	case "enter":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	case "esc":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEscape})
	case "up":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyUp})
	case "down":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyDown})
	case "left":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyLeft})
	case "right":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyRight})
	case "tab":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyTab})
	case "shift+tab":
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyTab, Mod: tea.ModShift})
	case "ctrl+a":
		return tea.KeyPressMsg(tea.Key{Code: 'a', Mod: tea.ModCtrl})
	default:
		r := []rune(spec)[0]
		return tea.KeyPressMsg(tea.Key{Code: r, Text: spec})
	}
}

// runCmd executes a command and returns its message (nil-safe).
func runCmd(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	return cmd()
}

var errBoom = errors.New("boom")
