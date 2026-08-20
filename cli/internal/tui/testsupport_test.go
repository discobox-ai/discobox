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

	harnesses    []Harness
	harnessErr   error
	configureErr error
	secrets      []HarnessSecret
	editChanged  bool

	listErr   error
	runErr    error
	createdID string

	// welcomed counts the times the window said the introduction had been
	// shown, and welcomeErr fails the write.
	welcomed   int
	welcomeErr error

	// runSteps is what Run reports as it goes, and provisionLines what the
	// provisioning watch reports; watched records which discoboxes the window
	// asked about.
	runSteps       []string
	provisionLines []string
	watched        []string

	openErr   error
	renameErr error
	editorErr error

	// execs is what the workspace's poll is told is running; execsErr fails
	// the listing, openExecErr every attach, openExecErrFor one exec's
	// attach, newShellErr the shell create.
	execs          []Exec
	execsErr       error
	openExecErr    error
	openExecErrFor map[string]error
	newShellErr    error
	// newTerminalErr fails the terminal create.
	newTerminalErr error
	// newShellID names the next exec NewShell creates, and newTerminalID the
	// next one NewTerminal does.
	newShellID    int
	newTerminalID int
	// terminalHarness is the harness the sandbox runs, which is what a
	// terminal it creates is reported with — and what puts it in the
	// workspace's left column.
	terminalHarness string

	// services is what the services menu is told the discobox declares,
	// servicesErr fails reading them, and serviceActs records every verb the
	// window ran, as "<verb> <sandbox> <service>".
	services    []Service
	servicesErr error
	serviceErr  error
	serviceActs []string

	// forward is what the workspace's port forward reports, and forwardErr
	// fails opening one. forwards counts the ones opened and closed, so a test
	// can hold the window to the rule that a workspace releases its ports.
	forward       *fakeForward
	forwardErr    error
	forwardsOpen  int
	forwardsClose int

	// draftErr fails every draft write.
	draftErr error

	// Calls, in order.
	drafts    []string // "folder prompt"
	runs      []RunRequest
	did       []string // "verb id"
	renames   []string // "id name"
	editors   []string // sandbox ids handed to VS Code
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
			Default: true, BuiltIn: true, Configurable: true, Image: "ghcr.io/example/claude:latest",
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
			BuiltIn: true, Configurable: true, Image: "ghcr.io/example/codex:latest",
			Run:     []string{"codex"},
			Secrets: []HarnessSecret{{Name: "OPENAI_API_KEY", Required: true, Declared: true}},
			Files:   []HarnessFile{{Path: "config.toml", Content: "{}", Configured: true}},
			Updated: now.Add(-30 * time.Minute),
		},
		{
			ID: "hc_custom", Name: "Custom", Slug: "custom", State: HarnessDisabled,
			Configurable: true, Image: "ghcr.io/example/custom:latest", Run: []string{"custom"},
			Updated: now.Add(-72 * time.Hour),
		},
		{
			ID: "hc_scratch", Name: "Scratch", Slug: "scratch", State: HarnessFailed,
			Configurable: true,
			Error:        "the setup exited before it finished", Updated: now.Add(-time.Minute),
		},
		{
			// The reserved built-in: no setup to run, so neither enabling nor
			// disabling applies to it. Born enabled because it needs nothing.
			ID: "hc_shell", Name: "Shell", Slug: "shell", State: HarnessEnabled,
			BuiltIn: true, Shell: true, Image: "ghcr.io/example/shell:latest",
			Updated: now.Add(-96 * time.Hour),
		},
	}
}

func (f *fakeSource) Session(context.Context) (Session, error) { return f.session, nil }

func (f *fakeSource) MarkWelcomed(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.welcomed++
	return f.welcomeErr
}

// SaveDraft records what the window handed the store, in order, so a test can
// see both what was saved and how often.
func (f *fakeSource) SaveDraft(_ context.Context, folder, prompt string) error {
	f.drafts = append(f.drafts, folder+" "+prompt)
	return f.draftErr
}

func (f *fakeSource) List(context.Context) ([]Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Sandbox(nil), f.sandboxes...), nil
}

func (f *fakeSource) Run(_ context.Context, req RunRequest, report func(string)) (Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, req)
	for _, step := range f.runSteps {
		report(step)
	}
	if f.runErr != nil {
		return Sandbox{}, f.runErr
	}
	return Sandbox{ID: f.createdID, Name: req.Prompt, State: StateStarting}, nil
}

// WatchProvisioning reports whatever the test staged and returns. The real one
// blocks until it is stopped, but it also reports nothing at all for a discobox
// that is already up — which is every discobox in these tests — so returning is
// the faithful behavior for a fake with nothing staged, and it keeps a window
// under test from waiting on a watch that will never say anything.
func (f *fakeSource) WatchProvisioning(_ context.Context, sandboxID string, report func(string)) {
	f.mu.Lock()
	f.watched = append(f.watched, sandboxID)
	lines := append([]string(nil), f.provisionLines...)
	f.mu.Unlock()
	for _, line := range lines {
		report(line)
	}
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

// openedEditors is the sandboxes handed to VS Code, read under the lock so a
// test driving the model on its own goroutines can look at it safely.
func (f *fakeSource) openedEditors() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.editors...)
}

func (f *fakeSource) OpenEditor(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editors = append(f.editors, id)
	return f.editorErr
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

func (f *fakeSource) Forward(context.Context, string) (Forward, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forwardErr != nil {
		return nil, f.forwardErr
	}
	f.forwardsOpen++
	if f.forward == nil {
		f.forward = newFakeForward()
	}
	f.forward.source = f
	return f.forward, nil
}

// fakeForward is a port forward the test drives: bind names a local port for a
// sandbox port and wakes the window exactly as a real bind does.
type fakeForward struct {
	source *fakeSource

	mu       sync.Mutex
	bindings []Binding
	closed   bool
	changed  chan struct{}
}

func newFakeForward(bindings ...Binding) *fakeForward {
	return &fakeForward{bindings: bindings, changed: make(chan struct{}, 1)}
}

func (f *fakeForward) Bindings() []Binding {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Binding(nil), f.bindings...)
}

func (f *fakeForward) Events() <-chan struct{} { return f.changed }

func (f *fakeForward) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.changed)
	if f.source != nil {
		f.source.mu.Lock()
		f.source.forwardsClose++
		f.source.mu.Unlock()
	}
	return nil
}

// bind adds a binding and wakes whoever is waiting, the way a forwarder that
// just took a local port does.
func (f *fakeForward) bind(binding Binding) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.bindings = append(f.bindings, binding)
	select {
	case f.changed <- struct{}{}:
	default:
	}
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

func (f *fakeSource) NewTerminal(_ context.Context, id string, cols, rows int) (Exec, Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.newTerminalErr != nil {
		return Exec{}, nil, f.newTerminalErr
	}
	f.newTerminalID++
	harness := f.terminalHarness
	if harness == "" {
		harness = "claude-code"
	}
	exec := Exec{
		ID:        fmt.Sprintf("exec_term%d", f.newTerminalID),
		Command:   []string{"claude"},
		Harness:   harness,
		Tty:       true,
		Live:      true,
		CreatedAt: time.Date(2026, 8, 7, 13, 0, f.newTerminalID, 0, time.UTC),
	}
	// The listing reports it from now on, the way the server would.
	f.execs = append(f.execs, exec)
	f.execOpens = append(f.execOpens, fmt.Sprintf("%s %s %dx%d", id, exec.ID, cols, rows))
	return exec, f.newExecTerminal(exec.ID), nil
}

func (f *fakeSource) Services(context.Context, string) ([]Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.servicesErr != nil {
		return nil, f.servicesErr
	}
	return append([]Service(nil), f.services...), nil
}

func (f *fakeSource) DoService(_ context.Context, verb ServiceVerb, sandboxID, serviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serviceErr != nil {
		return f.serviceErr
	}
	f.serviceActs = append(f.serviceActs, fmt.Sprintf("%s %s %s", verb, sandboxID, serviceID))
	return nil
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
	return f.configureErr
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
	// exit is the command's exit code, for a terminal standing in for a local
	// command. Nil is a terminal that is not one and has no result to give.
	exit *int
}

// ExitStatus makes this terminal an [ExitReporter] when a test has given it a
// result to report, and not one otherwise — the same split the real terminals
// have between a local command and a session in a discobox.
func (f *fakeTerminal) ExitStatus() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exit == nil {
		return 0, false
	}
	return *f.exit, true
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

// drainDepth bounds how far a chain of commands is followed. It is a guard
// against a cycle rather than a claim about how deep the window's command trees
// go, so it has room to spare: an operation that narrates itself batches its
// reporting alongside its work, which puts the work one level deeper than the
// batch and every message it produces deeper still.
const drainDepth = 8

func drain(t *testing.T, m *Model, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > drainDepth {
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
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "shift+backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModShift}
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
			ID: "sbx_one", Name: "fix flaky pool reaper tests", State: StateRunning, HasRuntime: true,
			Harness: "claude", Folder: "/src/disco2", Branch: "main", Commit: "a3f9c21", Dirty: true,
			Created: now.Add(-2 * time.Minute), Diff: DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7},
		},
		{
			ID: "sbx_two", Name: "exec/terminal consolidation", State: StateRunning, HasRuntime: true,
			Harness: "claude", Folder: "/src/disco2", Branch: "main", Commit: "a3f9c21",
			Created: now.Add(-18 * time.Minute), Upgrade: true,
			Diff: DiffStat{Known: true, Added: 903, Deleted: 511, Files: 24},
		},
		{
			ID: "sbx_three", Name: "openapi: sandbox upgrade field", State: StateStopped, HasRuntime: true,
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
