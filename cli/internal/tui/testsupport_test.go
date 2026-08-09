package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// fakeSource stands in for the server. Every method records what it was asked
// for, so a test can assert on the command a key press turned into rather than
// on the frame it drew.
type fakeSource struct {
	mu sync.Mutex

	session   Session
	sandboxes []Sandbox
	stats     map[string]DiffStat
	dirty     bool

	listErr   error
	runErr    error
	createdID string

	openErr   error
	renameErr error

	// Calls, in order.
	runs      []RunRequest
	did       []string // "verb id"
	renames   []string // "id name"
	interacts []string // "action id,id"
	opens     []string // "action id colsxrows"
	terminals []*fakeTerminal
}

func newFakeSource(sandboxes ...Sandbox) *fakeSource {
	return &fakeSource{
		session: Session{
			Project:        "default",
			DefaultProject: "default",
			Directory:      "/src/disco2",
			Branch:         "main",
			Harnesses:      []string{"claude", "codex"},
			DefaultHarness: "claude",
		},
		sandboxes: sandboxes,
		stats:     map[string]DiffStat{},
		createdID: "sbx_created",
	}
}

func (f *fakeSource) Session(context.Context) (Session, error) { return f.session, nil }

func (f *fakeSource) List(context.Context) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Sandbox(nil), f.sandboxes...), nil
}

func (f *fakeSource) DiffStat(_ context.Context, id string) (DiffStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stat, ok := f.stats[id]
	if !ok {
		return DiffStat{}, fmt.Errorf("no stat for %s", id)
	}
	return stat, nil
}

func (f *fakeSource) Run(_ context.Context, req RunRequest) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, req)
	if f.runErr != nil {
		return Sandbox{}, f.runErr
	}
	return Sandbox{ID: f.createdID, Name: req.Prompt, State: StateStarting}, nil
}

func (f *fakeSource) Dirty(context.Context, string) (bool, error) { return f.dirty, nil }

func (f *fakeSource) Do(_ context.Context, verb Verb, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.did = append(f.did, string(verb)+" "+id)
	return nil
}

func (f *fakeSource) Rename(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, id+" "+name)
	if f.renameErr != nil {
		return f.renameErr
	}
	for i, sb := range f.sandboxes {
		if sb.ID == id {
			f.sandboxes[i].Name = name
		}
	}
	return nil
}

func (f *fakeSource) Interact(_ context.Context, action Interaction, ids []string, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interacts = append(f.interacts, string(action)+" "+strings.Join(ids, ","))
	return nil
}

func (f *fakeSource) Open(_ context.Context, action Interaction, id string, cols, rows int) (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, fmt.Sprintf("%s %s %dx%d", action, id, cols, rows))
	if f.openErr != nil {
		return nil, f.openErr
	}
	term := newFakeTerminal()
	f.terminals = append(f.terminals, term)
	return term, nil
}

// fakeTerminal is a sandbox terminal on the end of a pipe: what a test sends
// lands on the pane's screen, and what the pane types lands in typed().
type fakeTerminal struct {
	out    chan []byte
	events chan TerminalEvent
	closed chan struct{}
	once   sync.Once

	mu    sync.Mutex
	input []byte
	sizes [][2]int
}

func newFakeTerminal() *fakeTerminal {
	return &fakeTerminal{
		out:    make(chan []byte, 16),
		events: make(chan TerminalEvent, 4),
		closed: make(chan struct{}),
	}
}

func (f *fakeTerminal) Read(p []byte) (int, error) {
	select {
	case chunk := <-f.out:
		return copy(p, chunk), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeTerminal) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.input = append(f.input, p...)
	return len(p), nil
}

func (f *fakeTerminal) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, [2]int{cols, rows})
	return nil
}

func (f *fakeTerminal) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTerminal) Events() <-chan TerminalEvent { return f.events }

func (f *fakeTerminal) send(s string) { f.out <- []byte(s) }

// typed is what the pane has sent to the sandbox, polled because input travels
// through the emulator on its own goroutine.
func (f *fakeTerminal) typed(want string) string {
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := string(f.input)
		f.mu.Unlock()
		if strings.Contains(got, want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fakeTerminal) size() [2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sizes) == 0 {
		return [2]int{}
	}
	return f.sizes[len(f.sizes)-1]
}

// newTestModel builds a window with color forced off, so a frame is plain text
// a test can read, and with a fixed clock so the age column never changes.
func newColorModel(t *testing.T, ds DataSource) *Model {
	t.Helper()
	m := newTestModel(t, ds)
	// The frame is rebuilt with color on, so what a real terminal is sent — the
	// row backgrounds especially — is what the test looks at.
	m.st = newStyles(true)
	return m
}

func newTestModel(t *testing.T, ds DataSource) *Model {
	t.Helper()
	// The whole window is built colorless, composer included, so a frame is
	// plain text a test can read.
	t.Setenv("NO_COLOR", "1")
	m := New(t.Context(), ds)
	m.logo = logo{}
	// Most of these are about the full launcher, which is what the window opens
	// out into. The opening prompt has its own tests, in compact_test.go.
	m.expanded = true
	m.list.now = func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) }
	// The runtime is what releases the terminal around an action; there is none
	// here, so the action is simply run and its result handed back.
	m.exec = func(c tea.ExecCommand, done tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			c.SetStdin(strings.NewReader(""))
			c.SetStdout(io.Discard)
			c.SetStderr(io.Discard)
			return done(c.Run())
		}
	}
	send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drain(t, m, m.Init(), 0)
	return m
}

// send drives the model the way the runtime would, feeding back the messages the
// commands it returns produce, so a frame can be looked at without a terminal.
//
// The timers — the cursor blink, the status expiry, the resize settle, the
// refresh tick — are given up on rather than waited for: they are sleeps, and
// waiting for them only buys a slower test.
func send(t *testing.T, m *Model, msgs ...tea.Msg) *Model {
	t.Helper()
	for _, msg := range msgs {
		_, cmd := m.Update(msg)
		drain(t, m, cmd, 0)
	}
	return m
}

func drain(t *testing.T, m *Model, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > 6 {
		return
	}
	msg, ok := runQuickly(cmd)
	if !ok || msg == nil {
		return
	}
	// A batch arrives as one message holding the commands it batched.
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			drain(t, m, sub, depth+1)
		}
		return
	}
	if _, quit := msg.(interface{ isQuit() }); quit {
		return
	}
	_, next := m.Update(msg)
	drain(t, m, next, depth+1)
}

func runQuickly(cmd tea.Cmd) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(20 * time.Millisecond):
		return nil, false
	}
}

// key builds the key press a terminal would send for a keystroke, in the two
// shapes the launcher has to cope with: a printable character carries its text,
// and everything else is a code.
func key(spec string) tea.KeyPressMsg {
	switch spec {
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case " ":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "ctrl+j":
		return tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "ctrl+left":
		return tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl}
	case "ctrl+right":
		return tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl}
	case "ctrl+b":
		return tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl}
	case "ctrl+a":
		return tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl}
	case "ctrl+d":
		return tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	case "f1":
		return tea.KeyPressMsg{Code: tea.KeyF1}
	}
	runes := []rune(spec)
	if len(runes) == 1 && runes[0] >= 'A' && runes[0] <= 'Z' {
		// A capital letter is a shifted one on the wire, which is the shape
		// keyName exists to see through.
		return tea.KeyPressMsg{Code: runes[0] + 32, ShiftedCode: runes[0], Text: spec, Mod: tea.ModShift}
	}
	return tea.KeyPressMsg{Code: runes[0], Text: spec}
}

func typeString(s string) []tea.Msg {
	msgs := make([]tea.Msg, 0, len(s))
	for _, r := range s {
		msgs = append(msgs, key(string(r)))
	}
	return msgs
}

// sizeMsg is a window resize.
func sizeMsg(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

// frame is the rendered window as plain lines, trailing blanks trimmed.
func frame(m *Model) []string {
	lines := strings.Split(m.View().Content, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return lines
}

// rawFrame is the rendered window with its padding intact, for the tests that
// are about the geometry rather than the words.
func rawFrame(m *Model) string { return m.View().Content }

// plainFrame is the rendered window as text, with every escape removed. It is
// what to read words out of: a cursor sitting on a character splits it from the
// rest of its line with escapes, so a substring match on the raw frame misses
// whatever the cursor happens to be on.
func plainFrame(m *Model) string { return ansi.Strip(rawFrame(m)) }

func frameText(m *Model) string { return strings.Join(frame(m), "\n") }

// showAllFolders drops the header's folder filter, which the window opens with
// set to the directory it is running in.
func showAllFolders(t *testing.T, m *Model) {
	t.Helper()
	send(t, m, key("tab"), key("up"), key("left"))
	if m.list.folder != "" {
		t.Fatalf("folder filter is %q, want every folder", m.list.folder)
	}
	send(t, m, key("down"))
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

func testSandboxes() []Sandbox {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return []Sandbox{
		{
			ID: "sbx_one", Name: "fix flaky pool reaper tests", State: StateRunning,
			Harness: "claude", Folder: "/src/disco2", Branch: "main", Commit: "a3f9c21", Dirty: true,
			LastUsed: now.Add(-2 * time.Minute), Diff: DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7},
		},
		{
			ID: "sbx_two", Name: "exec/terminal consolidation", State: StateRunning,
			Harness: "claude", Folder: "/src/disco2", Branch: "main", Commit: "a3f9c21",
			LastUsed: now.Add(-18 * time.Minute), Upgrade: true,
			Diff: DiffStat{Known: true, Added: 903, Deleted: 511, Files: 24},
		},
		{
			ID: "sbx_three", Name: "openapi: sandbox upgrade field", State: StateStopped,
			Harness: "codex", Folder: "/src/obot", Branch: "main", Commit: "1c713f6",
			LastUsed: now.Add(-time.Hour), Diff: DiffStat{Known: true},
		},
		{
			ID: "sbx_four", Name: "bats harness-configure endpoints", State: StateArchived,
			Harness: "codex", Folder: "/src/disco2", Branch: "main", Commit: "41a9507",
			LastUsed: now.Add(-48 * time.Hour), Diff: DiffStat{Known: true, Added: 240, Deleted: 96, Files: 11},
		},
	}
}
