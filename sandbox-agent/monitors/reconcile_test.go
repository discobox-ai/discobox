package monitors

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/execs/execstest"
	"github.com/discobox-ai/x/shorttmp"
)

func newTestReconciler(t *testing.T) (*Reconciler, *execstest.Units) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the exec runtime resolves guest paths; a Windows host has none")
	}
	root := t.TempDir()
	units := execstest.NewUnits(t)
	manager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: root,
		RuntimeDir:  filepath.Join(shorttmp.Dir(t), "rt"),
		Env:         map[string]string{"PATH": "/usr/bin", "SHELL": "/bin/sh"},
		Units:       units,
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	return &Reconciler{
		Execs:      manager,
		Dirs:       HomeDirs(home),
		Executable: "/opt/discobox/current/discobox-sandbox-agent",
		Home:       home,
	}, units
}

func monitorExecs(r *Reconciler) []execs.Exec {
	var out []execs.Exec
	for _, ex := range r.Execs.List() {
		if ex.Metadata[MetadataMonitorName] != "" {
			out = append(out, ex)
		}
	}
	return out
}

func TestReconcilerRunsOneRunnerPerEnabledInstance(t *testing.T) {
	r, units := newTestReconciler(t)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	if err := r.ensureEnabledDir(); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	if _, err := Enable(r.Dirs, Instance{Name: "pr-412", Monitor: "github-repo", Workdir: workdir}, false); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	r.Sync(ctx, logger)
	got := monitorExecs(r)
	if len(got) != 1 || len(units.Starts) != 1 {
		t.Fatalf("execs %+v after %d starts; want one, started once", got, len(units.Starts))
	}
	start := units.Starts[0]
	if start.Workdir != workdir || start.TTY {
		t.Fatalf("runner started in %q, tty %v", start.Workdir, start.TTY)
	}
	command := strings.Join(start.Command, " ")
	if !strings.Contains(command, "discobox-sandbox-agent") || !strings.Contains(command, RunnerCommand+" 'pr-412'") {
		t.Fatalf("runner command = %q", command)
	}
	if got[0].Metadata[MetadataMonitorID] != "github-repo" {
		t.Fatalf("metadata = %v", got[0].Metadata)
	}

	// Disabled, its runner is stopped and its exec removed.
	if err := Disable(r.Dirs, "pr-412"); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	if got := monitorExecs(r); len(got) != 0 || len(units.Stops) != 1 {
		t.Fatalf("after disable: execs %+v, %d stops", got, len(units.Stops))
	}
}

func TestReconcilerRelaunchesARunnerThatEnded(t *testing.T) {
	r, units := newTestReconciler(t)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	code := int64(1)
	units.Exit = &code
	if _, err := Enable(r.Dirs, Instance{Name: "ci", Monitor: "github-repo"}, false); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	first := monitorExecs(r)
	units.Exit = nil
	r.Sync(ctx, logger)
	second := monitorExecs(r)
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID || len(units.Starts) != 2 {
		t.Fatalf("first %+v, second %+v, %d starts; want one exec relaunched under its id", first, second, len(units.Starts))
	}
	// An instance naming no workdir, or one that is gone, runs in home.
	if units.Starts[0].Workdir != r.Home {
		t.Fatalf("workdir = %q, want home", units.Starts[0].Workdir)
	}
}

func TestReconcilerReplacesARunnerThatIsNotTheEnabledInstances(t *testing.T) {
	r, units := newTestReconciler(t)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	if _, err := Enable(r.Dirs, Instance{Name: "pr", Monitor: "github-repo", Options: map[string]string{"pr": "412"}}, false); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	first := monitorExecs(r)

	// Disabled and enabled with other options before any sync saw the gap.
	if err := Disable(r.Dirs, "pr"); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(r.Dirs, Instance{Name: "pr", Monitor: "github-repo", Options: map[string]string{"pr": "413"}}, false); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	second := monitorExecs(r)
	inst, _ := ReadInstance(r.Dirs, "pr")
	if len(second) != 1 || second[0].ID == first[0].ID || second[0].Metadata[MetadataMonitorInstance] != inst.ID {
		t.Fatalf("first %+v, second %+v; want the old runner replaced by one for %s", first, second, inst.ID)
	}

	// An agent that now starts runners from elsewhere — an exec recorded
	// before an upgrade — replaces the record rather than relaunching a
	// command whose binary may be gone.
	r.Executable = "/opt/discobox/current/features/core/discobox-sandbox-agent.new"
	r.Sync(ctx, logger)
	third := monitorExecs(r)
	if len(third) != 1 || third[0].ID == second[0].ID || !strings.Contains(strings.Join(third[0].Command, " "), r.Executable) {
		t.Fatalf("third %+v; want a runner started from %s", third, r.Executable)
	}
	if len(units.Starts) != 3 {
		t.Fatalf("%d starts, want 3", len(units.Starts))
	}
}

// A flood that overflows the kernel's watch queue — fsnotify's one error a
// test can provoke — is read past, and an enable after it is acted on in well
// under the resync. Unread, the error would hold back every later event.
func TestReconcilerReadsPastAWatchOverflow(t *testing.T) {
	r, _ := newTestReconciler(t)
	// Sized before Run starts: a skip once Run is held would never end it.
	perWriter := (watchQueueLimit(t)+2*4096)/2/8 + 1
	if err := r.ensureEnabledDir(); err != nil {
		t.Fatal(err)
	}
	// An unreadable instance makes each sync warn, and the warning is held
	// until the flood has overflowed the queue: Run reads nothing meanwhile.
	broken := filepath.Join(r.Dirs.Enabled, "Not A Name.json")
	if err := os.WriteFile(broken, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs := &heldLogs{release: make(chan struct{})}
	r.Logger = slog.New(logs)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-logs.held(t)

	// Files the reconciler ignores (no .json), each made and removed — at
	// least two events — until well past what fsnotify's buffer and the
	// kernel's queue can hold.
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				path := filepath.Join(r.Dirs.Enabled, fmt.Sprintf("flood-%d-%d", w, i))
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Error(err)
					return
				}
				_ = os.Remove(path)
			}
		}()
	}
	wg.Wait()
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	close(logs.release)
	// Enabled once the backlog is read, so no event of the flood's own
	// carries it: only an event after the overflow can.
	time.Sleep(2 * time.Second)

	if _, err := Enable(r.Dirs, Instance{Name: "after-flood", Monitor: "github-repo"}, false); err != nil {
		t.Fatal(err)
	}
	enabled := time.Now()
	for len(monitorExecs(r)) == 0 {
		if time.Since(enabled) > 10*time.Second {
			t.Fatalf("no runner %s after the enable: the watch stalled behind the flood (overflow seen: %v)", time.Since(enabled).Round(time.Millisecond), logs.overflowed())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Linux's inotify is where the overflow is certain; without one the test
	// proves only that the flood was drained.
	if runtime.GOOS == "linux" && !logs.overflowed() {
		t.Fatal("the flood never overflowed the watch queue, so the error path went untested")
	}
}

// watchQueueLimit is how many events the kernel queues for one watch
// before it overflows: Linux's limit, or its default where it cannot be read
// (no other system overflows, and the test asks only Linux for one). A host
// that raised it beyond what a test can flood skips the test.
func watchQueueLimit(t *testing.T) int {
	t.Helper()
	limit := 16384
	if raw, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			limit = n
		}
	}
	if limit > 1<<18 {
		t.Skipf("the kernel queues %d watch events; too many to overflow in a test", limit)
	}
	return limit
}

// heldLogs blocks the first record warning of an unreadable instance until
// release is closed, and notes whether the watch reported an error.
type heldLogs struct {
	release  chan struct{}
	once     sync.Once
	reached  chan struct{}
	mu       sync.Mutex
	watchErr bool
}

func (h *heldLogs) held(t *testing.T) <-chan struct{} {
	t.Helper()
	h.once.Do(func() { h.reached = make(chan struct{}) })
	return h.reached
}

func (h *heldLogs) overflowed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.watchErr
}

func (h *heldLogs) Enabled(context.Context, slog.Level) bool { return true }

func (h *heldLogs) Handle(_ context.Context, rec slog.Record) error {
	switch rec.Message {
	case "monitors: unreadable instance":
		h.once.Do(func() { h.reached = make(chan struct{}) })
		select {
		case <-h.reached:
		default:
			close(h.reached)
		}
		<-h.release
	case "monitors: watch the enabled directory":
		h.mu.Lock()
		h.watchErr = true
		h.mu.Unlock()
	}
	return nil
}

func (h *heldLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *heldLogs) WithGroup(string) slog.Handler      { return h }
