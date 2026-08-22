package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/x/shorttmp"
)

// blockingInstaller holds a launch inside install until the test releases it,
// which is what makes the boot-versus-attach window deterministic instead of
// something a test can only hit by luck.
//
// The gate is swapped between phases rather than closed once, so every field the
// installer goroutine reads is guarded: a test that reassigned the channel while
// EnsureInstalled was reading it raced, and the loser blocked until the package
// timeout rather than failing.
type blockingInstaller struct {
	entered chan struct{}

	mu      sync.Mutex
	release chan struct{}
	err     error

	calls atomic.Int32
}

func newBlockingInstaller() *blockingInstaller {
	return &blockingInstaller{
		entered: make(chan struct{}, 8),
		release: make(chan struct{}),
	}
}

func (b *blockingInstaller) EnsureInstalled(_ context.Context, _ config.Harness, _ string, _ map[string]string) error {
	b.calls.Add(1)
	b.entered <- struct{}{}
	b.mu.Lock()
	gate := b.release
	b.mu.Unlock()
	<-gate
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *blockingInstaller) RestoreSecretFiles(context.Context, config.Harness, map[string]string) ([]string, error) {
	return nil, nil
}

// hold closes nothing and installs a fresh gate, so the next install blocks.
func (b *blockingInstaller) hold() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.release = make(chan struct{})
}

// letGo releases whatever is waiting on the current gate.
func (b *blockingInstaller) letGo() {
	b.mu.Lock()
	gate := b.release
	b.mu.Unlock()
	close(gate)
}

func (b *blockingInstaller) failWith(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.err = err
}

// waitEntered blocks until an install starts, failing the test rather than
// hanging if none does.
func (b *blockingInstaller) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no install started")
	}
}

// expectNoSecondInstall proves a caller joined the launch in flight rather than
// starting its own, which would enter the installer again.
func (b *blockingInstaller) expectNoSecondInstall(t *testing.T, what string) {
	t.Helper()
	select {
	case <-b.entered:
		t.Fatalf("%s: a second install ran instead of joining the one in flight", what)
	case <-time.After(200 * time.Millisecond):
	}
}

// runInstall releases the one install the given work triggers, so a caller that
// only needs the work done does not have to choreograph the gate.
func (b *blockingInstaller) runInstall(done func()) {
	go func() {
		<-b.entered
		b.letGo()
		if done != nil {
			done()
		}
	}()
}

// countingPrimaryState is the durable "has the primary been launched" flag, with
// a count of how often it was read and written so a test can prove the
// read-modify-write happened once rather than racing.
type countingPrimaryState struct {
	mu       sync.Mutex
	launched bool
	reads    int
	marks    int
}

func (c *countingPrimaryState) PrimaryTerminalLaunched(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.launched, nil
}

func (c *countingPrimaryState) MarkPrimaryTerminalLaunched(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.marks++
	c.launched = true
	return nil
}

func newLaunchTestService(t *testing.T, installer Installer, state PrimaryStateStore, prompt []string) *Service {
	t.Helper()
	dir := shorttmp.Dir(t)
	units := &shimUnits{}
	t.Cleanup(units.Close)
	env := map[string]string{"PATH": "/usr/bin"}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         env,
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:        execManager,
		WorkingRoot:  dir,
		RuntimeDir:   filepath.Join(dir, "rt"),
		Env:          env,
		Harness:      config.Harness{ID: "codex", Command: []string{"codex"}, RelaunchCommand: []string{"codex", "resume"}},
		Units:        units,
		Installer:    installer,
		PrimaryState: state,
		Prompt:       prompt,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

func countPrimaries(list []execs.Exec) int {
	n := 0
	for _, e := range list {
		if IsPrimary(e) {
			n++
		}
	}
	return n
}

// Boot launches the primary terminal in a goroutine started just before the
// HTTP server serves, so an attach that no longer polls for a primary first
// (ADR 0039) arrives while boot is still installing. Both callers must land on
// one terminal: left as a check-then-act, the attach's scan runs before boot's
// record is visible and launches a second primary.
func TestConcurrentPrimaryLaunchesCollapseToOne(t *testing.T) {
	installer := newBlockingInstaller()
	state := &countingPrimaryState{}
	svc := newLaunchTestService(t, installer, state, []string{"fix", "the", "tests"})

	// Boot's launch, held inside install.
	bootErr := make(chan error, 1)
	go func() { bootErr <- svc.EnsurePrimary(context.Background(), svc.bootPrompt) }()
	installer.waitEntered(t)

	// The attach, arriving mid-install. It must join rather than launch, and it
	// must not come back until that launch is done.
	resolved := make(chan execs.Exec, 1)
	resolveErr := make(chan error, 1)
	go func() {
		exec, err := svc.ResolvePrimary(context.Background())
		resolved <- exec
		resolveErr <- err
	}()

	// Prove it did not start its own install: a second launch would enter the
	// installer again.
	installer.expectNoSecondInstall(t, "attach")

	// And prove it is still waiting. The exec record exists as "starting" from
	// the moment execs.Create writes it, before install runs and long before the
	// shim is listening, so a scan-and-return hands the attach a terminal whose
	// socket does not exist yet — the dial then burns its timeout on a terminal
	// that was never ready.
	select {
	case exec := <-resolved:
		t.Fatalf("ResolvePrimary returned %q while the harness was still installing", exec.ID)
	case <-time.After(200 * time.Millisecond):
	}

	installer.letGo()

	if err := <-bootErr; err != nil {
		t.Fatalf("boot EnsurePrimary: %v", err)
	}
	if err := <-resolveErr; err != nil {
		t.Fatalf("ResolvePrimary: %v", err)
	}
	exec := <-resolved

	if got := installer.calls.Load(); got != 1 {
		t.Fatalf("installer ran %d times, want 1", got)
	}
	if got := countPrimaries(svc.List()); got != 1 {
		t.Fatalf("primary terminals = %d, want 1", got)
	}
	if exec.ID == "" || !IsPrimary(exec) {
		t.Fatalf("ResolvePrimary returned %+v, want the launched primary", exec)
	}
	// The attach must connect to the terminal it waited behind, not to whatever
	// a fresh scan happens to pick.
	if live, ok := livePrimary(svc.List()); !ok || live.ID != exec.ID {
		t.Fatalf("ResolvePrimary returned %q, want the live primary %q", exec.ID, live.ID)
	}
}

// Both callers reading the launched flag before either marks it is what makes
// the user's prompt run twice: each takes primaryCreateRequest's first-launch
// arm and passes the prompt as argv, instead of one launching and one resuming.
func TestConcurrentPrimaryLaunchesRunThePromptOnce(t *testing.T) {
	installer := newBlockingInstaller()
	state := &countingPrimaryState{}
	prompt := []string{"fix", "the", "tests"}
	svc := newLaunchTestService(t, installer, state, prompt)

	done := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := svc.ResolvePrimary(context.Background())
			done <- err
		}()
	}
	installer.waitEntered(t)
	installer.letGo()
	for range 4 {
		if err := <-done; err != nil {
			t.Fatalf("ResolvePrimary: %v", err)
		}
	}

	if state.marks != 1 {
		t.Fatalf("launched flag written %d times, want 1", state.marks)
	}
	if got := countPrimaries(svc.List()); got != 1 {
		t.Fatalf("primary terminals = %d, want 1", got)
	}
	primary, ok := livePrimary(svc.List())
	if !ok {
		t.Fatal("no live primary")
	}
	// The prompt is typed into the terminal as the harness's arguments, so the
	// one terminal must carry it exactly once.
	seen := 0
	for _, arg := range primary.StartupCommand {
		if arg == prompt[0] {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("prompt appears %d times in %v, want 1", seen, primary.StartupCommand)
	}
}

// A caller that gives up — an attach whose client disconnected, or one that hit
// its own timeout — must not take the launch down with it. Boot and every other
// joiner are waiting on that install.
func TestPrimaryLaunchOutlivesTheCallerThatStartedIt(t *testing.T) {
	installer := newBlockingInstaller()
	svc := newLaunchTestService(t, installer, &countingPrimaryState{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := svc.ResolvePrimary(ctx)
		first <- err
	}()
	installer.waitEntered(t)

	// The starter walks away mid-install.
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned caller err = %v, want context.Canceled", err)
	}

	// A second caller joins the same launch and must still get a terminal.
	second := make(chan execs.Exec, 1)
	secondErr := make(chan error, 1)
	go func() {
		exec, err := svc.ResolvePrimary(context.Background())
		second <- exec
		secondErr <- err
	}()
	installer.expectNoSecondInstall(t, "second caller after the starter canceled")

	installer.letGo()
	if err := <-secondErr; err != nil {
		t.Fatalf("second ResolvePrimary: %v", err)
	}
	if exec := <-second; exec.ID == "" {
		t.Fatal("second caller got no terminal")
	}
	if got := installer.calls.Load(); got != 1 {
		t.Fatalf("installer ran %d times, want 1", got)
	}
}

// A launch that fails must be reported to everyone joined to it, and must clear
// the latch so the next attach retries instead of inheriting the failure.
func TestFailedPrimaryLaunchIsRetriedByTheNextCaller(t *testing.T) {
	installer := newBlockingInstaller()
	installer.failWith(errors.New("install exploded"))
	svc := newLaunchTestService(t, installer, &countingPrimaryState{}, nil)

	installer.runInstall(nil)
	if _, err := svc.ResolvePrimary(context.Background()); err == nil {
		t.Fatal("expected the failed install to surface")
	}

	// The next caller launches again rather than being handed the dead latch.
	installer.failWith(nil)
	installer.hold()
	installer.runInstall(nil)
	exec, err := svc.ResolvePrimary(context.Background())
	if err != nil {
		t.Fatalf("retry ResolvePrimary: %v", err)
	}
	if exec.ID == "" {
		t.Fatal("retry produced no terminal")
	}
}

// Reviving is a check-then-act like a first launch, so two attaches landing on
// the same dead terminal both decide to relaunch it. That is worse than
// duplicated work: both derive the next unit generation from the same stale
// record and so produce the same unit name, and the second one's socket removal
// — which exists to fence the *previous* run — deletes the socket the first
// one's shim has just bound, leaving a live run nothing can attach to.
//
// A terminal's id is its durable identity (ADR 0038), so the id is also the key
// concurrent revives collapse onto.
func TestConcurrentRevivesOfOneTerminalCollapseToOne(t *testing.T) {
	installer := newBlockingInstaller()
	svc := newLaunchTestService(t, installer, &countingPrimaryState{}, nil)

	// A terminal that has ended, which is what an attach arrives at. Create
	// blocks inside install, so the release has to come from another goroutine.
	firstInstall := make(chan struct{})
	installer.runInstall(func() { close(firstInstall) })
	created, err := svc.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Start(context.Background(), created.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	markExited(t, svc, created.ID)
	// The first phase's releaser must be done before the gate is re-armed, or
	// it would close the second phase's gate and the overlap would never happen.
	<-firstInstall

	// Two attaches revive it at once, held inside install so they genuinely
	// overlap.
	installer.hold()
	before := installer.calls.Load()
	results := make(chan execs.Exec, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			exec, err := svc.Revive(context.Background(), created.ID)
			results <- exec
			errs <- err
		}()
	}
	installer.waitEntered(t)
	// A second revive would enter the installer again.
	installer.expectNoSecondInstall(t, "concurrent revive")
	installer.letGo()

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Revive: %v", err)
		}
	}
	first, second := <-results, <-results
	if installed := installer.calls.Load() - before; installed != 1 {
		t.Fatalf("installer ran %d times for the revive, want 1", installed)
	}
	// Both callers get the same revived terminal, under the id they addressed.
	if first.ID != created.ID || second.ID != created.ID {
		t.Fatalf("revived ids = %q and %q, want both %q", first.ID, second.ID, created.ID)
	}
	if first.Unit != second.Unit {
		t.Fatalf("callers got different unit generations (%q vs %q): the terminal was relaunched twice", first.Unit, second.Unit)
	}
	// The surviving run's shim must still own its socket.
	if _, err := os.Stat(first.SocketPath); err != nil {
		t.Fatalf("revived terminal's socket is gone: %v", err)
	}
	// One live terminal, not two.
	live := 0
	for _, exec := range svc.List() {
		if exec.Status == execs.StatusStarting || exec.Status == execs.StatusRunning {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live terminals = %d, want 1", live)
	}
}
