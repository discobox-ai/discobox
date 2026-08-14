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
	dirty     bool

	harnesses   []Harness
	harnessErr  error
	secrets     []HarnessSecret
	editChanged bool

	listErr   error
	runErr    error
	createdID string

	openErr   error
	renameErr error

	// execs is what the workspace's poll is told is running; execsErr fails
	// the listing, openExecErr every attach, openExecErrFor one exec's
	// attach, newShellErr the shell create.
	execs          []Exec
	execsErr       error
	openExecErr    error
	openExecErrFor map[string]error
	newShellErr    error
	// newShellID names the next exec NewShell creates.
	newShellID int

	// Calls, in order.
	runs      []RunRequest
	did       []string // "verb id"
	renames   []string // "id name"
	interacts []string // "action id,id"
	opens     []string // "action id colsxrows"
	execOpens []string // "id execID colsxrows"
	terminals []*fakeTerminal
	// execTerminals is the terminal serving each exec attach, by exec id.
	execTerminals map[string]*fakeTerminal
	didHarness    []string // "verb id"
	configured    []string // harness id
	editedFiles   []string // "id path"
}

func newFakeSource(sandboxes ...Sandbox) *fakeSource {
	return &fakeSource{
		session: Session{
			Project:        "default",
			DefaultProject: "default",
			Directory:      "/src/disco2",
			Branch:         "main",
		},
		sandboxes: sandboxes,
		createdID: "sbx_created",
		harnesses: testHarnesses(),
	}
}

// testHarnesses is what a project usually looks like: the two harnesses the
// server ships, both set up and one of them the default, plus two the project
// registered itself — one never set up, one whose setup did not finish. The
// second row is what makes every action apply somewhere: `s` applies only to a
// harness that is enabled and not already the default.
func testHarnesses() []Harness {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return []Harness{
		{
			ID: "hc_claude", Name: "Claude", Slug: "claude", State: HarnessEnabled,
			Default: true, BuiltIn: true, Image: "ghcr.io/example/claude:latest",
			Run:     []string{"claude"},
			Secrets: []HarnessSecret{{Name: "ANTHROPIC_API_KEY", Required: true, Declared: true}},
			Files: []HarnessFile{
				{Path: ".claude.json", Content: "{}", Configured: true},
				{Path: "settings.json", Content: "{}"},
			},
			Updated: now.Add(-2 * time.Hour),
		},
		{
			ID: "hc_codex", Name: "Codex", Slug: "codex", State: HarnessEnabled,
			BuiltIn: true, Image: "ghcr.io/example/codex:latest",
			Run:     []string{"codex"},
			Secrets: []HarnessSecret{{Name: "OPENAI_API_KEY", Required: true, Declared: true}},
			Files:   []HarnessFile{{Path: "config.toml", Content: "{}", Configured: true}},
			Updated: now.Add(-30 * time.Minute),
		},
		{
			ID: "hc_custom", Name: "Custom", Slug: "custom", State: HarnessDisabled,
			Image: "ghcr.io/example/custom:latest", Run: []string{"custom"},
			Updated: now.Add(-72 * time.Hour),
		},
		{
			ID: "hc_scratch", Name: "Scratch", Slug: "scratch", State: HarnessFailed,
			Error: "the setup exited before it finished", Updated: now.Add(-time.Minute),
		},
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

func (f *fakeSource) Execs(context.Context, string) ([]Exec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execsErr != nil {
		return nil, f.execsErr
	}
	return append([]Exec(nil), f.execs...), nil
}

func (f *fakeSource) OpenExec(_ context.Context, id, execID string, cols, rows int) (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execOpens = append(f.execOpens, fmt.Sprintf("%s %s %dx%d", id, execID, cols, rows))
	if f.openExecErr != nil {
		return nil, f.openExecErr
	}
	if err := f.openExecErrFor[execID]; err != nil {
		return nil, err
	}
	return f.newExecTerminal(execID), nil
}

func (f *fakeSource) NewShell(_ context.Context, id string, cols, rows int) (Exec, Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newShellErr != nil {
		return Exec{}, nil, f.newShellErr
	}
	f.newShellID++
	exec := Exec{
		ID:        fmt.Sprintf("exec_shell%d", f.newShellID),
		Command:   []string{"/bin/zsh"},
		Tty:       true,
		Live:      true,
		CreatedAt: time.Date(2026, 8, 7, 12, 0, f.newShellID, 0, time.UTC),
	}
	// The listing reports it from now on, the way the server would.
	f.execs = append(f.execs, exec)
	f.execOpens = append(f.execOpens, fmt.Sprintf("%s %s %dx%d", id, exec.ID, cols, rows))
	return exec, f.newExecTerminal(exec.ID), nil
}

// newExecTerminal wires a fake terminal to an exec id; f.mu is held.
func (f *fakeSource) newExecTerminal(execID string) *fakeTerminal {
	term := newFakeTerminal()
	f.terminals = append(f.terminals, term)
	if f.execTerminals == nil {
		f.execTerminals = map[string]*fakeTerminal{}
	}
	f.execTerminals[execID] = term
	return term
}

// execTerm is the terminal serving one exec id's attach.
func (f *fakeSource) execTerm(execID string) *fakeTerminal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execTerminals[execID]
}

// addExec puts a session into the listing, as one started elsewhere would be.
func (f *fakeSource) addExec(exec Exec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, exec)
}

// execOpened is every attach asked for so far, as "sandbox exec colsxrows".
func (f *fakeSource) execOpened() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.execOpens...)
}

// endExec removes an exec from the listing, as an exited session would be.
func (f *fakeSource) endExec(execID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.execs {
		if f.execs[i].ID == execID {
			f.execs[i].Live = false
		}
	}
}

func (f *fakeSource) Harnesses(context.Context) ([]Harness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.harnessErr != nil {
		return nil, f.harnessErr
	}
	return append([]Harness(nil), f.harnesses...), nil
}

func (f *fakeSource) HarnessSecrets(_ context.Context, _ string) ([]HarnessSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HarnessSecret(nil), f.secrets...), nil
}

func (f *fakeSource) DoHarness(_ context.Context, verb HarnessVerb, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.didHarness = append(f.didHarness, string(verb)+" "+id)
	return nil
}

func (f *fakeSource) ConfigureHarness(_ context.Context, id string, _ io.Reader, _, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured = append(f.configured, id)
	return nil
}

func (f *fakeSource) EditHarnessFile(_ context.Context, id, path string, _ io.Reader, _, _ io.Writer) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editedFiles = append(f.editedFiles, id+" "+path)
	return f.editChanged, nil
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
	m.harnesses.now = m.list.now
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
	case "f3":
		return tea.KeyPressMsg{Code: tea.KeyF3}
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
			Created: now.Add(-2 * time.Minute), Diff: DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7},
		},
		{
			ID: "sbx_two", Name: "exec/terminal consolidation", State: StateRunning,
			Harness: "claude", Folder: "/src/disco2", Branch: "main", Commit: "a3f9c21",
			Created: now.Add(-18 * time.Minute), Upgrade: true,
			Diff: DiffStat{Known: true, Added: 903, Deleted: 511, Files: 24},
		},
		{
			ID: "sbx_three", Name: "openapi: sandbox upgrade field", State: StateStopped,
			Harness: "codex", Folder: "/src/obot", Branch: "main", Commit: "1c713f6",
			Created: now.Add(-time.Hour), Diff: DiffStat{Known: true},
		},
		{
			ID: "sbx_four", Name: "bats harness-configure endpoints", State: StateArchived,
			Harness: "codex", Folder: "/src/disco2", Branch: "main", Commit: "41a9507",
			Created: now.Add(-48 * time.Hour), Diff: DiffStat{Known: true, Added: 240, Deleted: 96, Files: 11},
		},
	}
}
