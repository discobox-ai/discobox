package terminal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestCaptureNotificationsFollowAtomicWritesAndDirectoryReplacement(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config", "nested", "settings.json")
	observed := make(chan string, 32)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchHarnessConfig(ctx, home, []string{"config/nested/settings.json"}, func() {
			content, _ := os.ReadFile(path)
			observed <- string(content)
		}, 20*time.Millisecond, time.Hour)
	}()
	// Wall-clock times, in FSNOTIFY_DEBUG's format, so a failure lines up
	// with the backend's own trace when that is on.
	now := func() string { return time.Now().Format("15:04:05.000000000") }
	// Wait on the condition, not a fixed per-step deadline: one Windows CI
	// run missed a step's 5s (#1810), and 400 iterations on a quiet Windows
	// machine never did, so how slow a loaded runner can be is not known.
	// Give up shortly before the test binary's own timeout, so a lost watch
	// still fails with the report below rather than a panic. That makes such
	// a failure slow; the backstop is an hour, so it cannot pass by polling.
	limit := 10 * time.Minute
	if deadline, ok := t.Deadline(); ok {
		limit = max(time.Until(deadline)-30*time.Second, time.Until(deadline)/2)
	}
	timeout := time.NewTimer(limit)
	defer timeout.Stop()
	wait := func(want string) {
		t.Helper()
		var seen []string
		for {
			select {
			case got := <-observed:
				if got == want {
					return
				}
				seen = append(seen, fmt.Sprintf("%q at %s", got, now()))
			case <-timeout.C:
				// Say whether the file was there to be captured (a lost
				// notification) or never landed (the write itself stalled).
				content, err := os.ReadFile(path)
				t.Fatalf("no notification captured %q by %s; observed meanwhile: %v; file now: %q (err %v)",
					want, now(), seen, content, err)
			}
		}
	}
	write := func(value string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".tmp", []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %q at %s", value, now())
	}
	wait("")       // Initial observation happens after watches are armed.
	write("first") // The entire directory tree was initially absent.
	wait("first")
	write("second") // Atomic replacement must not detach the watch.
	wait("second")
	// Deleting watched directories is where Windows' fsnotify backend sends
	// events and errors while servicing Remove; repeat it to catch a loop that
	// stops draining them (#1303).
	for i := range 50 {
		if err := os.RemoveAll(filepath.Join(home, "config")); err != nil {
			t.Fatal(err)
		}
		t.Logf("removed config at %s", now())
		wait("")
		value := fmt.Sprintf("third-%d", i)
		write(value)
		wait(value)
	}
	// Windows rejects renaming a directory with watched descendants;
	// fsnotify's rename-nested-watched-dir fixture excludes it too. Keep the
	// creation, atomic replacement, deletion and cancellation checks above/below.
	if runtime.GOOS != "windows" {
		// Moving a directory away leaves its inode alive. Drop descendant watches
		// on that old tree and follow the replacement at the original pathname.
		if err := os.Rename(filepath.Join(home, "config"), filepath.Join(home, "old-config")); err != nil {
			t.Fatal(err)
		}
		write("replacement")
		wait("replacement")
		write("replacement-edited")
		wait("replacement-edited")
	}
	cancel()
	select {
	case <-done:
	case <-timeout.C:
		t.Fatal("watcher did not stop")
	}
}

func TestCaptureNotificationIgnoresUnrelatedFiles(t *testing.T) {
	targets := []string{filepath.Join("home", "config", "settings.json")}
	for _, name := range []string{filepath.Join("home", "other"), filepath.Join("home", "config", "session.log"), filepath.Join("home", "config-other")} {
		if captureEventRelevant(name, targets) {
			t.Errorf("unrelated event %q triggers capture", name)
		}
	}
}

func TestCaptureWatchBackstop(t *testing.T) {
	home := t.TempDir()
	observed := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		watchHarnessConfig(ctx, home, nil, func() { observed <- struct{}{} }, time.Second, 20*time.Millisecond)
	}()
	for range 2 {
		select {
		case <-observed:
		case <-time.After(5 * time.Second):
			t.Fatal("missing initial or fallback scan")
		}
	}
}

// A removed directory's watches go with it, so a Remove must not be passed on
// as gone: dropping watches by path after the directory was recreated would
// drop the replacement's (#1810). A rename away still is.
func TestCaptureForwarderDropsWatchesOnlyForRenamedDirectories(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config")
	targets := []string{filepath.Join(config, "settings.json")}
	events := make(chan fsnotify.Event)
	errs := make(chan error)
	out := make(chan captureChange, 1)
	done := make(chan struct{})
	go forwardCaptureEvents(&fsnotify.Watcher{Events: events, Errors: errs}, targets, out, done)
	take := func() captureChange {
		t.Helper()
		select {
		case change := <-out:
			return change
		case <-time.After(5 * time.Second):
			t.Fatal("no change forwarded")
			return captureChange{}
		}
	}
	events <- fsnotify.Event{Name: config, Op: fsnotify.Remove}
	if change := take(); len(change.gone) != 0 {
		t.Fatalf("removal forwarded as gone: %v", change.gone)
	}
	events <- fsnotify.Event{Name: config, Op: fsnotify.Rename}
	if change := take(); len(change.gone) != 1 || change.gone[0] != config {
		t.Fatalf("rename forwarded as %v, want gone [%s]", change.gone, config)
	}
	close(events)
	close(errs)
	<-done
}
