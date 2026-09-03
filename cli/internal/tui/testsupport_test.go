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

	"github.com/discobox-ai/discobox/termpane"
)

// fakeSource stands in for the server. Every method records what it was asked
// for, so a test can assert on the command a key press turned into rather than
// on the frame it drew.
type fakeSource struct {
	resources Resources
	mu        sync.Mutex

	session   Session
	sandboxes []Sandbox
	workspace SourceWorkspace
	// runGate holds a create open until the test closes it. Nil is a create
	// that returns at once, which is what every test that is not about the
	// wait wants.
	runGate chan struct{}

	// measured records the directories the window asked to be measured, and
	// total is what the walk it gets back reports.
	measured []string
	total    DirectoryTotal
	stopped  int

	harnesses    []Harness
	harnessErr   error
	configureErr error
	secrets      []HarnessSecret
	editChanged  bool

	// The credential inbox: what is waiting, what can answer it, and what the
	// window did about it.
	requests        []CredentialRequest
	projectSecrets  []Secret
	requestsErr     error
	secretsErr      error
	approvals       []Approval
	denials         []string
	createdSecrets  []NewSecret
	approveErr      error
	createSecretErr error
	unbound         []string
	bound           []string
	unbindErr       error
	limited         []string
	limitErr        error
	updated         []SecretUpdate
	revalued        []string
	projectGrants   []Grant
	grantsErr       error
	revoked         []string
	createdGrants   []NewGrant
	createGrantErr  error
	deleted         []string
	deleteErr       error

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
	// newToolErr fails the tool create, endExecErr the kill that closing a
	// tool window does, and newToolID names the next exec NewTool creates.
	newToolErr error
	endExecErr error
	newToolID  int
	// toolFiles is the local copy of each tool file, keyed "tool/name", and
	// stands in for the config directory on this machine. editToolFile is what
	// $EDITOR would have written; nil leaves the file as it was.
	toolFiles    map[string]string
	editToolFile func(file ToolFile) string
	editToolErr  error
	// terminalHarness is the harness the sandbox runs, which is what a
	// terminal it creates is reported with — and what puts it in the
	// workspace's left column.
	terminalHarness string

	// services is what the services menu is told the discobox declares,
	// servicesErr fails reading them, and serviceActs records every verb the
	// window ran, as "<verb> <sandbox> <service>".
	services       []Service
	servicesErr    error
	serviceErr     error
	serviceActs    []string
	serviceLogs    map[string][]byte
	serviceLogsErr error

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
	toolRuns      []string // "tool argv"
	installed     []string // "tool/name → home" per file put into a discobox
	editedTools   []string // "tool/name" handed to $EDITOR
	ended         []string // exec ids ended by EndExec
	didHarness    []string // "verb id"
	configured    []string // harness id
	editedFiles   []string // "id path"
}

// promptText is the request's prompt as one string, which is what a test
// asserts on: the window sends one argument, and `discobox run` sends the words
// the shell split.
func promptText(req RunRequest) string { return strings.Join(req.Prompt, " ") }

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
			ConfigReminder: "Use /login, then /exit when sign-in is complete.", Run: []string{"codex"},
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

func (f *fakeSource) Resources(context.Context) (Resources, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resources, nil
}

func (f *fakeSource) setResources(r Resources) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resources = r
}

func (f *fakeSource) Run(_ context.Context, req RunRequest, report func(string)) (Sandbox, error) {
	f.mu.Lock()
	f.runs = append(f.runs, req)
	gate := f.runGate
	f.mu.Unlock()
	// A create held open, for a test that wants to look at what the window is
	// showing while one is underway. Nothing staged is a create that returns.
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, step := range f.runSteps {
		report(step)
	}
	if f.runErr != nil {
		return Sandbox{}, f.runErr
	}
	return Sandbox{ID: f.createdID, Name: promptText(req), State: StateStarting}, nil
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

func (f *fakeSource) Workspace(context.Context, string) (SourceWorkspace, error) {
	return f.workspace, nil
}

func (f *fakeSource) MeasureDirectory(_ context.Context, dir string) (func() DirectoryTotal, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.measured = append(f.measured, dir)
	return func() DirectoryTotal {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.total
		}, func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.stopped++
		}
}

func (f *fakeSource) measuredDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.measured...)
}

func (f *fakeSource) setTotal(total DirectoryTotal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total = total
}

func (f *fakeSource) stoppedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

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

// toolRunsSeen is the tool sessions asked for, and endedExecs the sessions
// ended, read under the lock for the same reason.
func (f *fakeSource) toolRunsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.toolRuns...)
}

func (f *fakeSource) endedExecs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ended...)
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

// setServices moves what the service poll reports while a workspace is up.
func (f *fakeSource) setServices(services []Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = services
}

// verbs is every discobox lifecycle verb the window has run.
func (f *fakeSource) verbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.did...)
}

// acts is every service verb the window has run.
func (f *fakeSource) acts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.serviceActs...)
}

func (f *fakeSource) ServiceLogs(_ context.Context, _, serviceID string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.serviceLogsErr != nil {
		return nil, f.serviceLogsErr
	}
	return f.serviceLogs[serviceID], nil
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

func (f *fakeSource) NewTool(_ context.Context, id string, spec ToolSpec, cols, rows int) (Exec, Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolRuns = append(f.toolRuns, spec.ID+" "+strings.Join(spec.Command, " "))
	if f.newToolErr != nil {
		return Exec{}, nil, f.newToolErr
	}
	// The files go in before the session, the way the real adapter does it.
	for _, file := range spec.Files {
		f.installed = append(f.installed, toolFileKeyOf(file)+" → "+file.Home)
	}
	f.newToolID++
	exec := Exec{
		ID:        fmt.Sprintf("exec_tool%d", f.newToolID),
		Command:   append([]string{}, spec.Command...),
		Tool:      spec.ID,
		Tty:       true,
		Live:      true,
		CreatedAt: time.Date(2026, 8, 7, 14, 0, f.newToolID, 0, time.UTC),
	}
	// The listing reports it from now on, the way the server would.
	f.execs = append(f.execs, exec)
	f.execOpens = append(f.execOpens, fmt.Sprintf("%s %s %dx%d", id, exec.ID, cols, rows))
	return exec, f.newExecTerminal(exec.ID), nil
}

// toolFileKeyOf names a tool file the way the fake's own map does.
func toolFileKeyOf(file ToolFile) string { return file.Tool + "/" + file.Name }

func (f *fakeSource) ToolFilePath(file ToolFile) string {
	return "/config/discobox/tools/" + toolFileKeyOf(file)
}

func (f *fakeSource) EditToolFile(_ context.Context, file ToolFile, _ io.Reader, _, _ io.Writer) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editedTools = append(f.editedTools, toolFileKeyOf(file))
	if f.editToolErr != nil {
		return false, f.editToolErr
	}
	if f.toolFiles == nil {
		f.toolFiles = map[string]string{}
	}
	key := toolFileKeyOf(file)
	before, seeded := f.toolFiles[key]
	if !seeded {
		// Created from the tool's default the first time, the way the real one
		// does — which is what makes the first edit open on something.
		before = file.Default
		f.toolFiles[key] = before
	}
	if f.editToolFile == nil {
		return false, nil
	}
	after := f.editToolFile(file)
	f.toolFiles[key] = after
	return after != before, nil
}

// installedFiles and editedToolFiles read those under the lock, for a test
// driving the model on its own goroutines.
func (f *fakeSource) installedFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.installed...)
}

func (f *fakeSource) editedToolFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.editedTools...)
}

func (f *fakeSource) EndExec(_ context.Context, _, execID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = append(f.ended, execID)
	if f.endExecErr != nil {
		return f.endExecErr
	}
	// The listing stops reporting it, the way the server would.
	for i, exec := range f.execs {
		if exec.ID == execID {
			f.execs = append(f.execs[:i], f.execs[i+1:]...)
			break
		}
	}
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

func (f *fakeSource) OpenHarnessConfigure(_ context.Context, id string, _, _ int) (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configured = append(f.configured, id)
	if f.configureErr != nil {
		return nil, f.configureErr
	}
	term := newFakeTerminal()
	f.terminals = append(f.terminals, term)
	return term, nil
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
// finishConfigure ends the configuration pane's terminal, which is what
// carries a setup flow's follow-up: the default the prompt promised to set,
// and the run that was waiting on the answer. Opening the pane is where
// configureHarnessThen now stops, so a test asserting on either has to close
// it first.
func finishConfigure(t *testing.T, m *Model) *Model {
	t.Helper()
	p := m.overlay
	if p == nil || p.configure == nil {
		t.Fatalf("no configuration pane is open to finish")
	}
	return send(t, m, paneMsg{id: p.id, msg: termpane.ClosedMsg{}})
}

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

// repaints reports whether a command carries Bubble Tea's whole-screen redraw.
//
// The message is unexported, so it is matched against the one tea.ClearScreen
// itself produces rather than by name; both are empty structs, and comparing
// them is the only handle a test has on it.
func repaints(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	msg, ok := runQuickly(cmd)
	if !ok || msg == nil {
		return false
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if repaints(sub) {
				return true
			}
		}
		return false
	}
	return msg == tea.ClearScreen()
}

func typeString(s string) []tea.Msg {
	msgs := make([]tea.Msg, 0, len(s))
	for _, r := range s {
		msgs = append(msgs, keyPress(string(r)))
	}
	return msgs
}

// sizeMsg is a window resize.
func sizeMsg(w, h int) tea.WindowSizeMsg { return tea.WindowSizeMsg{Width: w, Height: h} }

// frame is the rendered window as plain lines, trailing blanks trimmed.
// dialogText is the dialog as it is drawn, with the color taken off.
//
// The tests assert on what a person reads rather than on the field it was built
// from: a card's facts live in its sections as often as in its body, and a test
// against d.body would pass on a dialog that draws nothing.
func dialogText(m *Model) string {
	if m.dialog == nil {
		return ""
	}
	return ansi.Strip(m.dialog.view(m.st, &m.zones, 100, 40))
}

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
	send(t, m, keyPress("tab"), keyPress("up"), keyPress("left"))
	if m.list.folder != "" {
		t.Fatalf("folder filter is %q, want every folder", m.list.folder)
	}
	send(t, m, keyPress("down"))
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

func testSandboxes() []Sandbox {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return []Sandbox{
		{
			ID: "sbx_one", Name: "fix flaky pool reaper tests", State: StateRunning, HasRuntime: true,
			Harness: "claude", Folder: "/src/disco2", Source: "/src/disco2",
			Branch: "main", Commit: "a3f9c21", Dirty: true,
			Created: now.Add(-2 * time.Minute), Diff: DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7},
		},
		{
			ID: "sbx_two", Name: "exec/terminal consolidation", State: StateRunning, HasRuntime: true,
			Harness: "claude", Folder: "/src/disco2", Source: "/src/disco2",
			Branch: "main", Commit: "a3f9c21",
			Created: now.Add(-18 * time.Minute), Upgrade: true,
			Diff: DiffStat{Known: true, Added: 903, Deleted: 511, Files: 24},
		},
		{
			ID: "sbx_three", Name: "openapi: sandbox upgrade field", State: StateStopped, HasRuntime: true,
			Harness: "codex", Folder: "/src/obot", Source: "/src/obot",
			Branch: "main", Commit: "1c713f6",
			Created: now.Add(-time.Hour), Diff: DiffStat{Known: true},
		},
		{
			ID: "sbx_four", Name: "bats harness-configure endpoints", State: StateArchived,
			Harness: "codex", Folder: "/src/disco2",
			Source: "https://github.com/acme/foo", SourceRemote: true,
			Branch: "main", Commit: "41a9507",
			Created: now.Add(-48 * time.Hour), Diff: DiffStat{Known: true, Added: 240, Deleted: 96, Files: 11},
		},
	}
}

func (f *fakeSource) CredentialRequests(context.Context) ([]CredentialRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CredentialRequest(nil), f.requests...), f.requestsErr
}

func (f *fakeSource) Secrets(context.Context) ([]Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Secret(nil), f.projectSecrets...), f.secretsErr
}

func (f *fakeSource) CreateSecret(_ context.Context, secret NewSecret) (Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createSecretErr != nil {
		return Secret{}, f.createSecretErr
	}
	f.createdSecrets = append(f.createdSecrets, secret)
	created := Secret{ID: "sec_new", Name: secret.Name, Type: secret.Type, Host: secret.Host}
	f.projectSecrets = append(f.projectSecrets, created)
	return created, nil
}

func (f *fakeSource) ApproveCredentialRequest(_ context.Context, approval Approval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.approveErr != nil {
		return f.approveErr
	}
	f.approvals = append(f.approvals, approval)
	f.dropRequestLocked(approval.RequestID)
	return nil
}

func (f *fakeSource) DenyCredentialRequest(_ context.Context, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denials = append(f.denials, requestID)
	f.dropRequestLocked(requestID)
	return nil
}

// dropRequestLocked takes an answered request out of the inbox, so a poll after
// one is answered reports what the server would.
func (f *fakeSource) dropRequestLocked(requestID string) {
	kept := f.requests[:0]
	for _, req := range f.requests {
		if req.ID != requestID {
			kept = append(kept, req)
		}
	}
	f.requests = kept
}

// UpdateSecret records each part of an edit the way the server would apply it,
// so a test can assert on what the window asked for rather than on how many
// calls it took to ask.
func (f *fakeSource) UpdateSecret(_ context.Context, secretID string, update SecretUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if update.Host != nil && f.unbindErr != nil {
		return f.unbindErr
	}
	if update.MaxTTLSeconds != nil && f.limitErr != nil {
		return f.limitErr
	}
	f.updated = append(f.updated, update)
	if update.Host != nil {
		f.bound = append(f.bound, secretID+"="+*update.Host)
		if *update.Host == "" {
			f.unbound = append(f.unbound, secretID)
		}
	}
	if update.MaxTTLSeconds != nil {
		f.limited = append(f.limited, fmt.Sprintf("%s=%d", secretID, *update.MaxTTLSeconds))
	}
	if update.Value != nil {
		f.revalued = append(f.revalued, secretID)
	}
	for i := range f.projectSecrets {
		if f.projectSecrets[i].ID != secretID {
			continue
		}
		if update.Name != nil {
			f.projectSecrets[i].Name = *update.Name
		}
		if update.Host != nil {
			f.projectSecrets[i].Host = *update.Host
		}
		if update.MaxTTLSeconds != nil {
			f.projectSecrets[i].MaxTTL = time.Duration(*update.MaxTTLSeconds) * time.Second
		}
	}
	return nil
}

func (f *fakeSource) Grants(_ context.Context, secretID string) ([]Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.grantsErr != nil {
		return nil, f.grantsErr
	}
	var out []Grant
	for _, g := range f.projectGrants {
		if secretID == "" || g.SecretID == secretID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeSource) CreateGrant(_ context.Context, grant NewGrant) (Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createGrantErr != nil {
		return Grant{}, f.createGrantErr
	}
	f.createdGrants = append(f.createdGrants, grant)
	made := Grant{
		ID: "grant_new", SecretID: grant.SecretID, Scope: grant.Scope,
		ScopeKey: grant.ScopeKey, Host: grant.Host,
	}
	f.projectGrants = append(f.projectGrants, made)
	return made, nil
}

func (f *fakeSource) RevokeGrant(_ context.Context, grantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, grantID)
	kept := f.projectGrants[:0]
	for _, g := range f.projectGrants {
		if g.ID != grantID {
			kept = append(kept, g)
		}
	}
	f.projectGrants = kept
	return nil
}

func (f *fakeSource) DeleteSecret(_ context.Context, secretID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, secretID)
	kept := f.projectSecrets[:0]
	for _, s := range f.projectSecrets {
		if s.ID != secretID {
			kept = append(kept, s)
		}
	}
	f.projectSecrets = kept
	return nil
}

// hintLine is the key line as one string, the way the status line joins it.
// The hints themselves are key/label pairs now — a hint that names a key is a
// button for it — and what most of these tests read is the text.
func hintLine(hints []hint) string {
	text := make([]string, 0, len(hints))
	for _, h := range hints {
		text = append(text, h.text)
	}
	return strings.Join(text, hintSep)
}
