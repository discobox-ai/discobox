package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/discobox-ai/discobox/termpane"

	"github.com/discobox-ai/discobox/pkg/tools"
)

// The whole package renders colorless, so a frame is plain text a test can read
// back. It is set here rather than per test because it is a process-wide
// setting: os.Environ is what colorprofile reads (see detectColor), and a
// t.Setenv in each test would bar every one of them from running in parallel.
// A test that wants the colored frame builds its styles directly — newColorModel.
func TestMain(m *testing.M) {
	os.Setenv("NO_COLOR", "1")
	// A timer is told from work by what it returns: drain waits for work,
	// and leaves a timer unfired, as one set for later than the test looks.
	// Set once, before any test runs, so no test races another for it.
	tick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		return func() tea.Msg { return timerMsg{after: d, fire: fn} }
	}
	// A listener likewise: drain does not run one, and the driver does.
	listen = func(cmd tea.Cmd) tea.Cmd {
		return func() tea.Msg { return listenerMsg{run: cmd} }
	}
	os.Exit(m.Run())
}

// timerMsg is a timer the window set, unfired: drain does not deliver it.
// A test that wants what a timer carries calls fire.
type timerMsg struct {
	after time.Duration
	fire  func(time.Time) tea.Msg
}

// fakeSource stands in for the server. Every method records what it was asked
// for, so a test can assert on the command a key press turned into rather than
// on the frame it drew.
type fakeSource struct {
	harnessChanges        HarnessChanges
	harnessChangesWrites  []string
	harnessChangesSaveErr error
	harnessChangesForces  []bool
	resources             Resources
	mu                    sync.Mutex
	// skills is what the skills screen lists, and skillsErr what an add
	// fails with.
	skills    []SkillsEntry
	skillsErr error
	// renewals are the refresh requests answered, and renewErr what the next
	// answer fails with.
	renewals []Renewal
	renewErr error

	session   Session
	sandboxes []Sandbox
	// putImages are the images carried into a discobox by a paste, and
	// putImageErr what the next one fails with.
	putImages   []putImage
	putImageErr error
	// unreachable are the servers the listing reports it did not hear back
	// from, the way a window listing more than one server does.
	unreachable []string
	// listed is when the listing says each server last answered.
	listed    map[string]time.Time
	workspace SourceWorkspace
	// resolved records every source handed to ResolveSource, and sourceErr is
	// what it answers with — a directory that is not on this machine.
	resolved  []string
	sourceErr error
	// runGate holds a create open until the test closes it. Nil is a create
	// that returns at once, which is what every test that is not about the
	// wait wants.
	runGate chan struct{}
	// workspaceGate holds the check of the working tree open until the test
	// closes it, for a test about what the window shows meanwhile.
	workspaceGate chan struct{}
	// deliverGate holds a create open after the server has taken it, which is
	// where a real one delivers the source and syncs SSH config.
	deliverGate chan struct{}
	// deliverErr fails a create after the server has taken it, the way a
	// source push or an SSH config sync does.
	deliverErr error

	// measured records the directories the window asked to be measured, and
	// total is what the walk it gets back reports.
	measured []string
	total    DirectoryTotal
	stopped  int

	harnesses    []Harness
	harnessErr   error
	configureErr error
	// portsInUse is what LocalPortsInUse answers: the local ports a test says
	// something else on this machine already holds.
	portsInUse  map[int]bool
	secrets     []HarnessSecret
	editChanged bool

	// The credential inbox: what is waiting, what can answer it, and what the
	// window did about it.
	requests []CredentialRequest
	// Credentials an upstream refused: what the third band is drawn from.
	rejections     []SecretRejection
	rejectionsErr  error
	projectSecrets []Secret
	// secretsOn and harnessesOn are what a server named in the call holds,
	// for a window with several; one not named there answers with
	// projectSecrets and harnesses. onServer is every server-scoped call, as
	// "Method@server", which is what says where the window sent it.
	secretsOn       map[string][]Secret
	harnessesOn     map[string][]Harness
	onServer        []string
	requestsErr     error
	secretsErr      error
	approvals       []Approval
	trustApprovals  []TrustApproval
	trustDenials    []string
	denials         []string
	createdSecrets  []NewSecret
	approveErr      error
	createSecretErr error
	limited         []string
	limitErr        error
	updated         []SecretUpdate
	revalued        []string
	// projectBindings are the project's bindings; handedOn is how many
	// bindings a delete takes with it, by binding ID.
	projectBindings  []SecretBinding
	bindingsErr      error
	unbindings       []string
	handedOn         map[string]int
	createdBindings  []NewBinding
	createBindingErr error
	dismissed        []string
	deleted          []string
	deleteErr        error

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
	runWarnings    []string
	provisionLines []string
	watched        []string

	// delivered records which discoboxes the window asked to deliver the
	// source of before attaching, deliverSteps what that delivery reports, and
	// awaitedErr fails it. execOpensAtDelivery is how many sessions had been
	// opened when it was asked, which is none if it came first.
	delivered           []string
	execOpensAtDelivery []int
	deliverSteps        []string
	awaitedErr          error
	renameErr           error
	// hostToolErr fails RunHostTool, and toolsErr the catalog lookup.
	hostToolErr error
	toolsErr    error
	// catalog is every discobox's tools; nil is testTools.
	catalog []Tool

	// addressed records which discoboxes were looked up; addressErr fails the
	// lookup, and addresses is what a successful one reports.
	addressed  []string
	addressErr error
	addresses  Addresses

	// execs is what the workspace's poll is told is running; execsErr fails
	// the listing, openExecErr every attach, openExecErrFor one exec's
	// attach, newShellErr the shell create.
	execs       []Exec
	execsErr    error
	openExecErr error
	// failedExecSandboxes, when set, replaces the listing as an attach fails:
	// the server recording why a sandbox failed at the moment its attach does.
	failedExecSandboxes []Sandbox
	openExecErrFor      map[string]error
	newShellErr         error
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
	// pullDiffs are the files changed of a pull request, by its head, which
	// its session draws; pullReviews the sessions PullReview was asked for,
	// as "sandbox: base...head", and pullReviewErr fails it.
	pullDiffs     map[string]string
	pullReviews   []string
	pullReviewErr error
	// pullReviewGate, when set, holds every PullReview until it is closed.
	pullReviewGate chan struct{}
	editToolErr    error
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

	// audit is what a followed audit timeline delivers: a test sends on it
	// and FollowAudit hands each update on. auditFollows records which
	// discoboxes were followed and auditStops how many follows have ended;
	// auditDetails is what AuditDetail answers, by record ID.
	audit        chan AuditUpdate
	auditFollows []string
	auditStops   int
	auditDetails map[string]string
	auditReads   []string
	// auditBodies are the recordings AuditBody answers, by "ID/part"; a
	// record has on its card the parts it has here.
	auditBodies map[string]string

	// issues are what Issue answers, by "repository#number", and issueErr
	// fails every read; issueReads counts the reads. comments records every
	// comment posted, as "repository#number: body", and commentErr fails
	// posting one.
	issues     map[string]Issue
	issueErr   error
	issueReads int
	comments   []string
	commentErr error
	// states records every close and reopen, as "repository#number:
	// state/reason", and stateErr fails one.
	states   []string
	stateErr error
	// merges records every merge, as "repository#number: method@sha", and
	// mergeErr fails one.
	merges   []string
	mergeErr error

	// forward is what the workspace's port forward reports, and forwardErr
	// fails opening one. forwards counts the ones opened and closed, so a test
	// can hold the window to the rule that a workspace releases its ports.
	forward       *fakeForward
	forwardErr    error
	forwardsOpen  int
	forwardsClose int
	// forwardIDs is the discobox each forward was asked for, in order.
	forwardIDs []string

	// draftErr fails every draft write.
	draftErr error

	// The workspace's automatic push: pushes is what each call answers with,
	// pushErr fails the call as a whole, and pushCalls records every call as
	// "<id> <held slug=commit,…>" so a test can hold the window to what it
	// asked for and what it said it was already holding.
	pushes    []SourcePush
	pushErr   error
	pushCalls []string

	// Calls, in order.
	drafts      []string
	views       []ListView
	lastSources []string
	// nothingSaved is a machine no window has narrowed yet, which opens on
	// every server, folder and tag rather than on its own folder.
	nothingSaved bool
	runs         []RunRequest
	did          []string      // "verb id"
	doErr        error         // what every lifecycle verb answers
	doGate       chan struct{} // when set, every lifecycle verb waits for it to close
	renames      []string      // "id name"
	// titles is what each renamed box was showing before it was given a name,
	// which is what clearing the name shows again.
	titles      map[string]string
	editors     []editorOpen // the sandboxes handed to a host tool, and which one
	toolLookups []string     // the sandboxes whose tools were asked for
	execOpens   []string     // "id execID colsxrows"
	terminals   []*fakeTerminal
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

// testHostID is the machine the test window is running on, which is what makes
// a discobox created on another one recognizable as such.
const testHostID = "host_0123456789abcdef"

// testKey is the origin key a discobox cut from root on the test window's
// machine is filed under, and testHostKey is the machine's own, which files
// the ones with no source (ADR 0111). The CLI works both out; the window only
// reads them.
func testKey(root string) string { return originkey.Of(testHostID, root) }

var testHostKey = originkey.Host(testHostID)

// farHostID is another machine, and farHostKey files its discoboxes that have
// no source — a folder this window can list but has nothing to cut from.
const farHostID = "host_wilma000000000"

var farHostKey = originkey.Host(farHostID)

// cutFrom files a test sandbox as one this machine cut from root.
func cutFrom(s Sandbox, root string) Sandbox {
	s.Source, s.OriginKey, s.SourceOriginKey, s.OriginHostID = root, testKey(root), testKey(root), testHostID
	return s
}

func newFakeSource(sandboxes ...Sandbox) *fakeSource {
	return &fakeSource{
		session: Session{
			Project:        "default",
			DefaultProject: "default",
			Directory:      "/src/disco2",
			Branch:         "main",
			HostID:         testHostID,
			OriginKey:      testKey("/src/disco2"),
			HostKey:        testHostKey,
		},
		sandboxes: sandboxes,
		titles:    map[string]string{},
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
			Default: true, BuiltIn: true, Configurable: true, Bundle: "ghcr.io/example/claude:latest",
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
			BuiltIn: true, Configurable: true, Bundle: "ghcr.io/example/codex:latest",
			ConfigReminder: "Use /login, then /exit when sign-in is complete.", Run: []string{"codex"},
			Secrets: []HarnessSecret{{Name: "OPENAI_API_KEY", Required: true, Declared: true}},
			Files:   []HarnessFile{{Path: "config.toml", Content: "{}", Configured: true}},
			Updated: now.Add(-30 * time.Minute),
		},
		{
			ID: "hc_custom", Name: "Custom", Slug: "custom", State: HarnessDisabled,
			Configurable: true, Bundle: "ghcr.io/example/custom:latest", Run: []string{"custom"},
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
			BuiltIn: true, Shell: true, Bundle: "ghcr.io/example/shell:latest",
			Updated: now.Add(-96 * time.Hour),
		},
	}
}

// Session is the fake's session. Unless a test says the folder is a fresh one,
// it hands back a view saved on the window's own folder: most tests are about
// one folder's discoboxes, which is what a window left narrowed there opens on.
func (f *fakeSource) Session(context.Context) (Session, error) {
	s := f.session
	if !f.nothingSaved && s.View.IsZero() {
		own := s.folder()
		s.View = ListView{FolderKey: own.key, FolderLabel: own.label, FolderSource: own.source, FolderLocal: own.local}
	}
	return s, nil
}

func (f *fakeSource) MarkWelcomed(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.welcomed++
	return f.welcomeErr
}

// SaveDraft records what the window handed the store, in order, so a test can
// see both what was saved and how often.
func (f *fakeSource) SaveDraft(_ context.Context, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drafts = append(f.drafts, prompt)
	return f.draftErr
}

// savedDrafts is every draft written so far.
func (f *fakeSource) savedDrafts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.drafts...)
}

// forwardsAskedFor is the discobox of every forward asked for so far.
func (f *fakeSource) forwardsAskedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.forwardIDs...)
}

// SaveView records the filters the window handed the store, in order.
func (f *fakeSource) SaveView(_ context.Context, view ListView) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.views = append(f.views, view)
	return nil
}

// SaveLastSource records each source a create was cut from, in order.
func (f *fakeSource) SaveLastSource(_ context.Context, source string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSources = append(f.lastSources, source)
	return nil
}

func (f *fakeSource) List(context.Context) (Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return Listing{}, f.listErr
	}
	return Listing{
		Sandboxes:   append([]Sandbox(nil), f.sandboxes...),
		Unreachable: append([]string(nil), f.unreachable...),
		Listed:      maps.Clone(f.listed),
	}, nil
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

// runRequests is every create the window has asked for. A create runs on the
// driver's own goroutine, so a test waiting for one reads it through here
// rather than off the field.
func (f *fakeSource) runRequests() []RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RunRequest(nil), f.runs...)
}

func (f *fakeSource) Run(_ context.Context, req RunRequest, report, warn func(string), accepted func()) (Sandbox, error) {
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
	for _, step := range f.runSteps {
		report(step)
	}
	for _, warning := range f.runWarnings {
		warn(warning)
	}
	if err := f.runErr; err != nil {
		f.mu.Unlock()
		return Sandbox{}, err
	}
	accepted()
	deliver, deliverErr := f.deliverGate, f.deliverErr
	created := Sandbox{ID: f.createdID, Name: promptText(req), State: StateStarting}
	f.mu.Unlock()
	if deliver != nil {
		<-deliver
	}
	if deliverErr != nil {
		return created, deliverErr
	}
	return created, nil
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

func (f *fakeSource) DeliverSource(_ context.Context, sandboxID string, report func(string), _ CredentialUI) error {
	f.mu.Lock()
	f.delivered = append(f.delivered, sandboxID)
	f.execOpensAtDelivery = append(f.execOpensAtDelivery, len(f.execOpens))
	steps := append([]string(nil), f.deliverSteps...)
	err := f.awaitedErr
	f.mu.Unlock()
	for _, step := range steps {
		report(step)
	}
	return err
}

func (f *fakeSource) Workspace(context.Context, string) (SourceWorkspace, error) {
	if f.workspaceGate != nil {
		<-f.workspaceGate
	}
	return f.workspace, nil
}

// ResolveSource answers the way the real one does for a directory that is
// there: what was typed, unchanged. sourceErr is what a test stages for one
// that is not.
func (f *fakeSource) ResolveSource(_ context.Context, source string) (Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, source)
	if f.sourceErr != nil {
		return Source{}, f.sourceErr
	}
	// Filed under the directory the ref sits in, which is what the real one's
	// repository root comes to for a directory that is one.
	dir := source
	if parsed, err := sourceref.Parse(source); err == nil {
		dir = parsed.Location
	}
	return Source{Value: source, Directory: dir, OriginKey: testKey(dir)}, nil
}

// setSourceErr moves what ResolveSource answers while the window is up, so a
// test can refuse a path and then accept the correction.
func (f *fakeSource) setSourceErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sourceErr = err
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

func (f *fakeSource) Do(_ context.Context, verb Verb, id string, _ CredentialUI) error {
	if f.doGate != nil {
		<-f.doGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.did = append(f.did, string(verb)+" "+id)
	return f.doErr
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
			// What the server does with a meta name: it is what the row shows
			// when set, and the title is shown again when it is cleared.
			if f.sandboxes[i].GivenName == "" {
				f.titles[id] = sb.Name
			}
			f.sandboxes[i].GivenName = name
			f.sandboxes[i].Name = name
			if name == "" {
				f.sandboxes[i].Name = f.titles[id]
			}
		}
	}
	return nil
}

// editorOpen is one RunHostTool call: which sandbox, and which tool it was
// handed to.
type editorOpen struct {
	id   string
	tool string
}

// openedEditors is the sandboxes handed to a host tool, read under the lock so
// a test driving the model on its own goroutines can look at it safely.
func (f *fakeSource) openedEditors() []editorOpen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]editorOpen(nil), f.editors...)
}

// testTools is the catalog a discobox offers in these tests: the image's two
// and the CLI's two, the way the real declarations have them.
func testTools() []Tool {
	return []Tool{
		{ID: tools.DiffID, Key: "d", Label: "diff", Detail: "what has changed, in discobox-review"},
		{ID: "fresh", Key: "f", Label: "fresh", Detail: "the fresh editor, in the box", Files: []ToolFile{
			{Tool: "fresh", Name: "config.jsonc", Home: ".config/fresh/config.json", Default: "// vim: set ft=jsonc :\n{}\n"},
			{Tool: "fresh", Name: "live_diff.json", Home: ".local/share/fresh/orchestrator/state/live_diff.json", Default: "{}\n"},
		}},
		{ID: "vscode", Key: "v", Label: "vscode", Detail: "open the discobox in VS Code, in a window of its own", Host: true},
		{ID: "zed", Key: "z", Label: "zed", Detail: "open the discobox in Zed, in a window of its own", Host: true},
	}
}

func (f *fakeSource) tools() []Tool {
	if f.catalog == nil {
		return testTools()
	}
	return f.catalog
}

func (f *fakeSource) Tools(_ context.Context, id string) ([]Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolLookups = append(f.toolLookups, id)
	if f.toolsErr != nil {
		return nil, f.toolsErr
	}
	return f.tools(), nil
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

func (f *fakeSource) PushSources(_ context.Context, id string, held map[string]string) ([]SourcePush, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	holds := make([]string, 0, len(held))
	for slug, commit := range held {
		holds = append(holds, slug+"="+commit)
	}
	sort.Strings(holds)
	f.pushCalls = append(f.pushCalls, strings.TrimSpace(id+" "+strings.Join(holds, ",")))
	if f.pushErr != nil {
		return nil, f.pushErr
	}
	pushes := make([]SourcePush, 0, len(f.pushes))
	for _, push := range f.pushes {
		// A source held at what it names now is resolved and not sent again,
		// which is what the data source does with a push that already failed.
		if commit, ok := held[push.Slug]; ok && commit == push.Commit {
			push.Err, push.Pushed = nil, false
		}
		pushes = append(pushes, push)
	}
	return pushes, nil
}

// pushedCalls is every automatic push the window has made so far.
func (f *fakeSource) pushedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pushCalls...)
}

func (f *fakeSource) RunHostTool(_ context.Context, id, toolID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.editors = append(f.editors, editorOpen{id: id, tool: toolID})
	return f.hostToolErr
}

func (f *fakeSource) Addresses(_ context.Context, id string) (Addresses, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addressed = append(f.addressed, id)
	if f.addressErr != nil {
		return Addresses{}, f.addressErr
	}
	if f.addresses == (Addresses{}) {
		return Addresses{SSH: "ssh " + id, Git: "ssh://" + id + "/home/discobox/repo"}, nil
	}
	return f.addresses, nil
}

func (f *fakeSource) addressLookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.addressed...)
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
		if f.failedExecSandboxes != nil {
			f.sandboxes = f.failedExecSandboxes
		}
		return nil, f.openExecErr
	}
	if err := f.openExecErrFor[execID]; err != nil {
		return nil, err
	}
	return f.newExecTerminal(execID), nil
}

func (f *fakeSource) Forward(_ context.Context, id string) (Forward, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forwardErr != nil {
		return nil, f.forwardErr
	}
	f.forwardsOpen++
	f.forwardIDs = append(f.forwardIDs, id)
	// A forward closed is gone: the next one asked for is a new one, as the
	// real data source's is.
	if f.forward == nil || f.forward.isClosed() {
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

func (f *fakeForward) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// Close counts itself on the source after letting go of its own lock: the
// source asks a forward whether it is closed while holding the source's.
func (f *fakeForward) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	close(f.changed)
	f.mu.Unlock()
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

func (f *fakeSource) Issue(_ context.Context, repository string, number int, _ bool) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueReads++
	if f.issueErr != nil {
		return Issue{}, f.issueErr
	}
	issue, ok := f.issues[fmt.Sprintf("%s#%d", repository, number)]
	if !ok {
		return Issue{}, errors.New("GitHub answered 404: Not Found")
	}
	return issue, nil
}

func (f *fakeSource) CommentOnIssue(_ context.Context, repository string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, fmt.Sprintf("%s#%d: %s", repository, number, body))
	return nil
}

// PullReview starts the files changed as a session, which draws the diff
// pullDiffs has for the head, as discobox-review would over it, and records
// what it was asked for.
func (f *fakeSource) PullReview(_ context.Context, id string, pull PullRequest, cols, rows int) (Exec, Terminal, error) {
	f.mu.Lock()
	gate := f.pullReviewGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullReviewErr != nil {
		return Exec{}, nil, f.pullReviewErr
	}
	f.pullReviews = append(f.pullReviews, fmt.Sprintf("%s: %s...%s", id, pull.BaseSHA, pull.HeadSHA))
	f.newToolID++
	exec := Exec{
		ID:           fmt.Sprintf("exec_files%d", f.newToolID),
		Command:      []string{"discobox-review"},
		FilesChanged: true,
		Tty:          true,
		Live:         true,
		CreatedAt:    time.Date(2026, 8, 7, 14, 0, f.newToolID, 0, time.UTC),
	}
	// The listing reports it from now on, the way the server would.
	f.execs = append(f.execs, exec)
	f.execOpens = append(f.execOpens, fmt.Sprintf("%s %s %dx%d", id, exec.ID, cols, rows))
	term := f.newExecTerminal(exec.ID)
	term.send(strings.ReplaceAll(f.pullDiffs[pull.HeadSHA], "\n", "\r\n"))
	return exec, term, nil
}

func (f *fakeSource) MergePullRequest(_ context.Context, repository string, number int, method, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.merges = append(f.merges, fmt.Sprintf("%s#%d: %s@%s", repository, number, method, sha))
	key := fmt.Sprintf("%s#%d", repository, number)
	if issue, ok := f.issues[key]; ok && issue.Pull != nil {
		pull := *issue.Pull
		pull.Merged, pull.MergedBy, pull.MergedAt = true, "me", time.Now()
		issue.State, issue.Pull = "closed", &pull
		f.issues[key] = issue
	}
	return nil
}

func (f *fakeSource) SetIssueState(_ context.Context, repository string, number int, state, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return f.stateErr
	}
	f.states = append(f.states, fmt.Sprintf("%s#%d: %s/%s", repository, number, state, reason))
	key := fmt.Sprintf("%s#%d", repository, number)
	if issue, ok := f.issues[key]; ok {
		issue.State, issue.StateReason = state, reason
		if state == "open" {
			issue.StateReason = ""
		}
		f.issues[key] = issue
	}
	return nil
}

func (f *fakeSource) FollowAudit(ctx context.Context, sandboxID string, report func(AuditUpdate)) error {
	f.mu.Lock()
	f.auditFollows = append(f.auditFollows, sandboxID)
	feed := f.audit
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.auditStops++
		f.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update := <-feed:
			report(update)
		}
	}
}

func (f *fakeSource) AuditDetail(_ context.Context, _, recordID string) (AuditRecordDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auditReads = append(f.auditReads, recordID)
	text, ok := f.auditDetails[recordID]
	if !ok {
		return AuditRecordDetail{}, fmt.Errorf("no audit record %s", recordID)
	}
	detail := AuditRecordDetail{Text: text}
	for _, part := range []string{AuditRequestBody, AuditResponseBody, AuditStream} {
		if _, ok := f.auditBodies[recordID+"/"+part]; ok {
			detail.Recordings = append(detail.Recordings, part)
		}
	}
	return detail, nil
}

func (f *fakeSource) AuditBody(_ context.Context, _, recordID, part string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auditReads = append(f.auditReads, recordID+"/"+part)
	body, ok := f.auditBodies[recordID+"/"+part]
	if !ok {
		return "", fmt.Errorf("no %s recorded for %s", part, recordID)
	}
	return body, nil
}

func (f *fakeSource) auditState() (follows []string, stops int, reads []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auditFollows...), f.auditStops, append([]string(nil), f.auditReads...)
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

func (f *fakeSource) NewTool(_ context.Context, id, toolID string, cols, rows int) (Exec, Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolRuns = append(f.toolRuns, toolID)
	if f.newToolErr != nil {
		return Exec{}, nil, f.newToolErr
	}
	// The files go in before the session, the way the real adapter does it.
	for _, t := range f.tools() {
		if t.ID != toolID {
			continue
		}
		for _, file := range t.Files {
			f.installed = append(f.installed, toolFileKeyOf(file)+" → "+file.Home)
		}
	}
	f.newToolID++
	exec := Exec{
		ID:        fmt.Sprintf("exec_tool%d", f.newToolID),
		Command:   []string{toolID},
		Tool:      toolID,
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

// putImage is one image a pane carried into a discobox.
type putImage struct {
	sandboxID, name string
	data            []byte
}

func (f *fakeSource) PutImage(_ context.Context, sandboxID, name string, data []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putImageErr != nil {
		return "", f.putImageErr
	}
	f.putImages = append(f.putImages, putImage{sandboxID: sandboxID, name: name, data: data})
	return "/tmp/discobox-paste/" + name, nil
}

func (f *fakeSource) putImagesSoFar() []putImage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]putImage(nil), f.putImages...)
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

func (f *fakeSource) Harnesses(_ context.Context, server string) ([]Harness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "Harnesses@"+server)
	if f.harnessErr != nil {
		return nil, f.harnessErr
	}
	if on, ok := f.harnessesOn[server]; ok {
		return append([]Harness(nil), on...), nil
	}
	return append([]Harness(nil), f.harnesses...), nil
}

func (f *fakeSource) HarnessSecrets(_ context.Context, server, _ string) ([]HarnessSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "HarnessSecrets@"+server)
	return append([]HarnessSecret(nil), f.secrets...), nil
}

func (f *fakeSource) DoHarness(_ context.Context, server string, verb HarnessVerb, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "DoHarness@"+server)
	f.didHarness = append(f.didHarness, string(verb)+" "+id)
	return nil
}

func (f *fakeSource) OpenHarnessConfigure(_ context.Context, server, id string, _, _ int) (Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "OpenHarnessConfigure@"+server)
	f.configured = append(f.configured, id)
	if f.configureErr != nil {
		return nil, f.configureErr
	}
	term := newFakeTerminal()
	f.terminals = append(f.terminals, term)
	return term, nil
}

// configuredHarnesses is which harnesses had their setup opened, in order.
func (f *fakeSource) configuredHarnesses() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.configured...)
}

func (f *fakeSource) LocalPortsInUse(_ context.Context, ports []int) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var inUse []int
	for _, port := range ports {
		if f.portsInUse[port] {
			inUse = append(inUse, port)
		}
	}
	return inUse
}

func (f *fakeSource) EditHarnessFile(_ context.Context, server, id, path string, _ io.Reader, _, _ io.Writer) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "EditHarnessFile@"+server)
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

	mu       sync.Mutex
	input    []byte
	sizes    [][2]int
	repaints int
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

func (f *fakeTerminal) Repaint() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repaints++
	return nil
}

func (f *fakeTerminal) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTerminal) Events() <-chan TerminalEvent { return f.events }

func (f *fakeTerminal) send(s string) { f.out <- []byte(s) }

// asked is how many repaints the pane has asked this terminal for, and the size
// it last re-asserted asking.
func (f *fakeTerminal) asked() (int, [2]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sizes) == 0 {
		return f.repaints, [2]int{}
	}
	return f.repaints, f.sizes[len(f.sizes)-1]
}

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
	// plain text a test can read. NO_COLOR is what does it, set once for the
	// package in TestMain.
	m := New(t.Context(), ds)
	m.logo = logo{}
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
	// A test starts in the composer whatever the listing holds, which is where
	// a window is when a key beat the first listing to it (openOnList). Nearly
	// every test here is about something a few keys in from there, and says
	// which keys; where a window opens untouched is newOpeningModel's.
	m.opened = true
	send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drain(t, m, m.Init(), 0)
	return m
}

// newOpeningModel builds a window nobody has touched yet and loads it, so a
// test can look at where it opened.
func newOpeningModel(t *testing.T, ds DataSource) *Model {
	t.Helper()
	m := New(t.Context(), ds)
	m.logo = logo{}
	send(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	drain(t, m, m.Init(), 0)
	return m
}

// finishConfigure ends the configuration pane's terminal, which is what
// carries a setup flow's follow-up: the default the prompt promised to set,
// and the run that was waiting on the answer. Opening the pane is where
// configureHarnessThen stops, so a test asserting on either has to close
// it first.
func finishConfigure(t *testing.T, m *Model) *Model {
	t.Helper()
	p := m.overlay
	if p == nil || p.configure == nil {
		t.Fatalf("no configuration pane is open to finish")
	}
	return send(t, m, paneMsg{id: p.id, msg: termpane.ClosedMsg{}})
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

// drain runs a command and every command its messages lead to, delivering
// each message to the model. A timer is left unfired (timerMsg) and a
// listener unrun (listenerMsg): both wait for what no step of a test brings.
// A test that has queued what a listener waits for drains its run, which is
// then work like any; the tests that follow a listener drive the window
// instead (driver_test.go), which runs it as the runtime does.
func drain(t *testing.T, m *Model, cmd tea.Cmd, depth int) {
	t.Helper()
	if cmd == nil || depth > drainDepth {
		return
	}
	msg := run(t, cmd)
	if msg == nil {
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
	if _, timer := msg.(timerMsg); timer {
		return
	}
	if _, waits := asListener(msg); waits {
		return
	}
	_, next := m.Update(msg)
	drain(t, m, next, depth+1)
}

// listenerMsg is a listener the window started, not run: what listen
// returns in a test, as tick returns a timerMsg.
type listenerMsg struct{ run tea.Cmd }

// asListener reads a listener out of a message: one the window started, or
// one a pane started, which arrives wrapped as the pane's (fromPane). Run,
// the second answers as the pane's.
func asListener(msg tea.Msg) (listenerMsg, bool) {
	switch msg := msg.(type) {
	case listenerMsg:
		return msg, true
	case paneMsg:
		inner, ok := msg.msg.(listenerMsg)
		if !ok {
			return listenerMsg{}, false
		}
		return listenerMsg{run: func() tea.Msg {
			got := inner.run()
			if got == nil {
				return nil
			}
			return paneMsg{id: msg.id, msg: got}
		}}, true
	case reviewMsg:
		// The files changed' session, tagged to its window as a pane's is.
		inner, ok := msg.msg.(listenerMsg)
		if !ok {
			return listenerMsg{}, false
		}
		return listenerMsg{run: func() tea.Msg {
			got := inner.run()
			if got == nil {
				return nil
			}
			return reviewMsg{pane: msg.pane, window: msg.window, msg: got}
		}}, true
	}
	return listenerMsg{}, false
}

// commandWait is how long a command is given to finish. Every command a
// test runs answers from a fake, so a working one never reaches it, however
// loaded the machine: it is long so a slow run is not mistaken for a broken
// one, and a command that reaches it fails the test, named — a wait nobody
// wrapped in listen, or a fake that never answers.
const commandWait = 10 * time.Second

// run runs one command to its end and returns its message.
func run(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	msg, err := runFor(cmd, commandWait)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// runFor runs one command, waiting up to wait for it, and says which
// command did not finish. The goroutine it leaves is the bug the error
// names.
func runFor(cmd tea.Cmd, wait time.Duration) (tea.Msg, error) {
	if libraryTimers[commandName(cmd)] {
		// Not run at all: a timer fires later than a test looks, and this
		// one, the library's own, touches nothing a test holds.
		return nil, nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, nil
	case <-time.After(wait):
		return nil, fmt.Errorf("command %s did not finish in %s: a command that waits by design goes through listen, a timer through tick", commandName(cmd), wait)
	}
}

// libraryTimers are the timers the window's widgets set themselves, which do
// not go through tick: told apart by what they are, as tick's are by what
// they return. A cursor's blink is one per focused field.
var libraryTimers = map[string]bool{
	"charm.land/bubbles/v2/cursor.(*Model).Blink.func1": true,
}

// commandName is the function a command is, which names where it was made.
func commandName(cmd tea.Cmd) string {
	if fn := runtime.FuncForPC(reflect.ValueOf(cmd).Pointer()); fn != nil {
		return fn.Name()
	}
	return "an unnamed command"
}

// repaints reports whether a command carries Bubble Tea's whole-screen redraw.
//
// The message is unexported, so it is matched against the one tea.ClearScreen
// itself produces rather than by name; both are empty structs, and comparing
// them is the only handle a test has on it.
func repaints(t *testing.T, cmd tea.Cmd) bool {
	t.Helper()
	if cmd == nil {
		return false
	}
	msg := run(t, cmd)
	if msg == nil {
		return false
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, sub := range batch {
			if repaints(t, sub) {
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

// tightFrame is plainFrame with every run of spaces made one, for asserting
// what a row of a table says without restating its column widths.
func tightFrame(m *Model) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(plainFrame(m), "\n", " \n ")), " ")
}

func frameText(m *Model) string { return strings.Join(frame(m), "\n") }

// showAllFolders drops the header's folder filter, which the window opens with
// set to the directory it is running in.
func showAllFolders(t *testing.T, m *Model) {
	t.Helper()
	send(t, m, keyPress("tab"))
	filterTo(t, m, allFolders)
	if m.list.folder.key != "" {
		t.Fatalf("folder filter is %q, want every folder", m.list.folder.label)
	}
	if m.focus == focusFilter {
		send(t, m, keyPress("down"))
	}
	if m.focus != focusList {
		t.Fatalf("focus = %v, want the list", m.focus)
	}
}

// filterTo opens the header's filter card and marks the choice reading each
// label, then applies them, the way a person would: ↑ ↓ to the row, Space to
// mark it, Enter to show what is marked (the cursor rests on the last mark). A label is matched on the first row
// reading it, and "Group: label" names the group where two could read alike.
func filterTo(t *testing.T, m *Model, labels ...string) {
	t.Helper()
	m.dialog = m.filterDialog()
	for _, label := range labels {
		markFilter(t, m, label)
	}
	send(t, m, keyPress("enter"))
	if m.dialog != nil {
		t.Fatalf("the filter card is still up after Enter")
	}
}

// markFilter moves the open filter card's cursor to the row reading label and
// marks it with Space.
func markFilter(t *testing.T, m *Model, label string) {
	t.Helper()
	if m.dialog == nil || m.dialog.kind != dlgFilter {
		t.Fatal("the filter card is not open")
	}
	group, want, grouped := strings.Cut(label, ": ")
	if !grouped {
		want = label
	}
	p := m.dialog.filter
	at := -1
	var seen []string
	for i, row := range p.rows() {
		seen = append(seen, row.group+": "+row.label)
		if row.label == want && (!grouped || row.group == group) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("the filter card has no %q among %q", label, seen)
	}
	for p.cursor < at {
		send(t, m, keyPress("down"))
	}
	for p.cursor > at {
		send(t, m, keyPress("up"))
	}
	send(t, m, keyPress(" "))
}

func testSandboxes() []Sandbox {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	return []Sandbox{
		{
			ID: "sbx_one", Name: "fix flaky pool reaper tests", TerminalTitle: "fix flaky pool reaper tests", State: StateRunning, HasRuntime: true,
			Harness: "claude", OriginKey: testKey("/src/disco2"), OriginHostID: testHostID,
			Source: "/src/disco2", SourceOriginKey: testKey("/src/disco2"),
			Branch: "main", Commit: "a3f9c21", Dirty: true,
			Created: now.Add(-2 * time.Minute), Diff: DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7},
		},
		{
			ID: "sbx_two", Name: "exec/terminal consolidation", TerminalTitle: "exec/terminal consolidation", State: StateRunning, HasRuntime: true,
			Harness: "claude", OriginKey: testKey("/src/disco2"), OriginHostID: testHostID,
			Source: "/src/disco2", SourceOriginKey: testKey("/src/disco2"),
			Branch: "main", Commit: "a3f9c21",
			Created: now.Add(-18 * time.Minute), Upgrade: true,
			Diff: DiffStat{Known: true, Added: 903, Deleted: 511, Files: 24},
		},
		{
			ID: "sbx_three", Name: "openapi: sandbox upgrade field", TerminalTitle: "openapi: sandbox upgrade field", State: StateStopped, HasRuntime: true,
			Harness: "codex", OriginKey: testKey("/src/obot"), OriginHostID: testHostID,
			Source: "/src/obot", SourceOriginKey: testKey("/src/obot"),
			Branch: "main", Commit: "1c713f6",
			Created: now.Add(-time.Hour), Diff: DiffStat{Known: true},
		},
		{
			ID: "sbx_four", Name: "bats harness-configure endpoints", TerminalTitle: "bats harness-configure endpoints", State: StateArchived,
			Harness: "codex", OriginKey: testKey("https://github.com/acme/foo"), OriginHostID: testHostID,
			Source: "https://github.com/acme/foo", SourceRemote: true, SourceOriginKey: testKey("https://github.com/acme/foo"),
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

func (f *fakeSource) SecretRejections(context.Context) ([]SecretRejection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SecretRejection(nil), f.rejections...), f.rejectionsErr
}

func (f *fakeSource) Secrets(_ context.Context, server string) ([]Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "Secrets@"+server)
	if on, ok := f.secretsOn[server]; ok {
		return append([]Secret(nil), on...), f.secretsErr
	}
	return append([]Secret(nil), f.projectSecrets...), f.secretsErr
}

func (f *fakeSource) CreateSecret(_ context.Context, server string, secret NewSecret) (Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "CreateSecret@"+server)
	if f.createSecretErr != nil {
		return Secret{}, f.createSecretErr
	}
	f.createdSecrets = append(f.createdSecrets, secret)
	created := Secret{ID: "sec_new", Name: secret.Name, Type: secret.Type, Routes: secret.Routes}
	f.projectSecrets = append(f.projectSecrets, created)
	return created, nil
}

func (f *fakeSource) ApproveCredentialRequest(_ context.Context, server string, approval Approval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "ApproveCredentialRequest@"+server)
	if f.approveErr != nil {
		return f.approveErr
	}
	f.approvals = append(f.approvals, approval)
	f.dropRequestLocked(approval.RequestID)
	return nil
}

func (f *fakeSource) DenyCredentialRequest(_ context.Context, server, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "DenyCredentialRequest@"+server)
	f.denials = append(f.denials, requestID)
	f.dropRequestLocked(requestID)
	return nil
}

func (f *fakeSource) DismissRefresh(_ context.Context, server, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "DismissRefresh@"+server)
	f.dismissed = append(f.dismissed, requestID)
	f.dropRequestLocked(requestID)
	return nil
}

func (f *fakeSource) RefreshSecret(_ context.Context, server string, renewal Renewal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "RefreshSecret@"+server)
	if f.renewErr != nil {
		return f.renewErr
	}
	f.renewals = append(f.renewals, renewal)
	f.dropRequestLocked(renewal.RequestID)
	return nil
}

func (f *fakeSource) ApproveTrustRequest(_ context.Context, server string, approval TrustApproval) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "ApproveTrustRequest@"+server)
	if f.approveErr != nil {
		return f.approveErr
	}
	f.trustApprovals = append(f.trustApprovals, approval)
	f.dropRequestLocked(approval.RequestID)
	return nil
}

func (f *fakeSource) DenyTrustRequest(_ context.Context, server, requestID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "DenyTrustRequest@"+server)
	f.trustDenials = append(f.trustDenials, requestID)
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
func (f *fakeSource) UpdateSecret(_ context.Context, server, secretID string, update SecretUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "UpdateSecret@"+server)
	if update.MaxTTLSeconds != nil && f.limitErr != nil {
		return f.limitErr
	}
	f.applySecretUpdateLocked(secretID, update)
	return nil
}

// applySecretUpdateLocked records a change to a secret and makes it, whether it
// came as an edit or with an approval.
func (f *fakeSource) applySecretUpdateLocked(secretID string, update SecretUpdate) {
	f.updated = append(f.updated, update)
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
		if update.MaxTTLSeconds != nil {
			f.projectSecrets[i].MaxTTL = time.Duration(*update.MaxTTLSeconds) * time.Second
		}
		if update.Routes != nil {
			f.projectSecrets[i].Routes = *update.Routes
		}
	}
}

func (f *fakeSource) Bindings(_ context.Context, server string, filter BindingFilter) ([]SecretBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "Bindings@"+server)
	if f.bindingsErr != nil {
		return nil, f.bindingsErr
	}
	var out []SecretBinding
	for _, b := range f.projectBindings {
		if filter.SecretID == "" || b.SecretID == filter.SecretID {
			out = append(out, b)
		}
	}
	return out, nil
}

func (f *fakeSource) CreateBinding(_ context.Context, server string, binding NewBinding) (SecretBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "CreateBinding@"+server)
	if f.createBindingErr != nil {
		return SecretBinding{}, f.createBindingErr
	}
	f.createdBindings = append(f.createdBindings, binding)
	made := SecretBinding{
		ID: "binding_new", SecretID: binding.SecretID, TargetKind: binding.TargetKind,
		TargetID: binding.TargetID, Use: binding.Use, Delegate: binding.Delegate,
		EnvVar: binding.EnvVar, Routes: binding.Routes,
	}
	f.projectBindings = append(f.projectBindings, made)
	return made, nil
}

func (f *fakeSource) DeleteBinding(_ context.Context, server, bindingID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "DeleteBinding@"+server)
	f.unbindings = append(f.unbindings, bindingID)
	kept := f.projectBindings[:0]
	for _, b := range f.projectBindings {
		if b.ID != bindingID && b.ParentID != bindingID {
			kept = append(kept, b)
		}
	}
	f.projectBindings = kept
	return f.handedOn[bindingID], nil
}

func (f *fakeSource) Skills(_ context.Context, server string) (SkillsListing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "Skills@"+server)
	return SkillsListing{Entries: append([]SkillsEntry(nil), f.skills...), ClientFile: "/home/me/.config/discobox/client.yaml"}, nil
}

// AddSkills files a value the way the real one does by its form: one with a
// scheme is the project's.
func (f *fakeSource) AddSkills(_ context.Context, server, value string) (SkillsEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "AddSkills@"+server)
	if f.skillsErr != nil {
		return SkillsEntry{}, f.skillsErr
	}
	entry := SkillsEntry{Value: value}
	if strings.Contains(value, "://") {
		entry.Project, entry.ID = true, "skills_"+itoa(len(f.skills)+1)
	}
	f.skills = append(f.skills, entry)
	return entry, nil
}

func (f *fakeSource) RemoveSkills(_ context.Context, server string, entry SkillsEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "RemoveSkills@"+server)
	kept := f.skills[:0]
	for _, e := range f.skills {
		if e != entry {
			kept = append(kept, e)
		}
	}
	f.skills = kept
	return nil
}

func (f *fakeSource) DeleteSecret(_ context.Context, server, secretID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "DeleteSecret@"+server)
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
// The hints themselves are key/label pairs — a hint that names a key is a
// button for it — and what most of these tests read is the text.
func hintLine(hints []hint) string {
	text := make([]string, 0, len(hints))
	for _, h := range hints {
		text = append(text, h.text)
	}
	return strings.Join(text, hintSep)
}

// calls is every server-scoped call the window has made so far, as
// "Method@server".
func (f *fakeSource) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.onServer...)
}

// A command that blocks and was not wrapped as a listener or a timer fails
// the test that ran it, by name, rather than hanging it: a wait added
// without listen is found the first time a test runs it.
func TestABlockingCommandIsNamed(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block)
	_, err := runFor(waitsUnwrapped(block), 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "waitsUnwrapped") {
		t.Fatalf("err = %v, want the blocking command named", err)
	}
	if msg, err := runFor(listen(waitsUnwrapped(block)), 50*time.Millisecond); err != nil {
		t.Fatalf("a listener was run: %v", err)
	} else if _, ok := msg.(listenerMsg); !ok {
		t.Fatalf("msg = %#v, want the listener handed back unrun", msg)
	}
}

// waitsUnwrapped is a command that waits on block, as a listener nobody
// marked would.
func waitsUnwrapped(block chan struct{}) tea.Cmd {
	return func() tea.Msg {
		<-block
		return nil
	}
}

func (f *fakeSource) HarnessChanges(_ context.Context, id string) (HarnessChanges, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onServer = append(f.onServer, "HarnessChanges@"+id)
	return f.harnessChanges, nil
}
func (f *fakeSource) SaveHarnessChanges(_ context.Context, id, candidate string, force bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.harnessChangesWrites = append(f.harnessChangesWrites, "save "+id+" "+candidate)
	f.harnessChangesForces = append(f.harnessChangesForces, force)
	if !force {
		return "", f.harnessChangesSaveErr
	}
	return "saved", nil
}
func (f *fakeSource) DiscardHarnessChanges(_ context.Context, id, candidate string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.harnessChangesWrites = append(f.harnessChangesWrites, "discard "+id+" "+candidate)
	return "discarded", nil
}

func readySandboxes() []Sandbox {
	all := testSandboxes()
	all[0].Dirty = false
	all[0].Git = GitState{Known: true, Branch: "main", Commit: "b7d0f11"}
	all[0].Diff = DiffStat{Known: true, Added: 142, Deleted: 38, Files: 7}
	return all
}
