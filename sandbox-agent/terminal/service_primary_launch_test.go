package terminal

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

// blockingInstaller holds a launch inside install until the test releases it,
// which is what makes the boot-versus-attach window deterministic instead of
// something a test can only hit by luck.
type blockingInstaller struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	err     error
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
	<-b.release
	return b.err
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
	dir := t.TempDir()
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
	select {
	case <-installer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("boot launch never reached install")
	}

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
	select {
	case <-installer.entered:
		t.Fatal("attach started a second primary launch instead of joining the one in flight")
	case <-time.After(200 * time.Millisecond):
	}

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

	close(installer.release)

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
	select {
	case <-installer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no launch reached install")
	}
	close(installer.release)
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
	select {
	case <-installer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("launch never reached install")
	}

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
	select {
	case <-installer.entered:
		t.Fatal("second caller started its own launch; the first caller's cancel killed the one in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(installer.release)
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
	installer.err = errors.New("install exploded")
	svc := newLaunchTestService(t, installer, &countingPrimaryState{}, nil)

	if _, err := svc.ResolvePrimaryOnce(t, installer); err == nil {
		t.Fatal("expected the failed install to surface")
	}

	// The next caller launches again rather than being handed the dead latch.
	installer.err = nil
	installer.release = make(chan struct{})
	go func() {
		<-installer.entered
		close(installer.release)
	}()
	exec, err := svc.ResolvePrimary(context.Background())
	if err != nil {
		t.Fatalf("retry ResolvePrimary: %v", err)
	}
	if exec.ID == "" {
		t.Fatal("retry produced no terminal")
	}
}

// ResolvePrimaryOnce runs a single launch to completion, releasing the blocking
// installer as soon as it is entered.
func (s *Service) ResolvePrimaryOnce(t *testing.T, installer *blockingInstaller) (execs.Exec, error) {
	t.Helper()
	go func() {
		<-installer.entered
		close(installer.release)
	}()
	return s.ResolvePrimary(context.Background())
}
