package terminal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	wait := func(want string) {
		t.Helper()
		timeout := time.NewTimer(5 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case got := <-observed:
				if got == want {
					return
				}
			case <-timeout.C:
				t.Fatalf("no notification captured %q", want)
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
	}
	wait("")       // Initial observation happens after watches are armed.
	write("first") // The entire directory tree was initially absent.
	wait("first")
	write("second") // Atomic replacement must not detach the watch.
	wait("second")
	if err := os.RemoveAll(filepath.Join(home, "config")); err != nil {
		t.Fatal(err)
	}
	wait("")
	write("third")
	wait("third")
	// Moving a directory away leaves its inode alive. Drop descendant watches
	// on that old tree and follow the replacement at the original pathname.
	if err := os.Rename(filepath.Join(home, "config"), filepath.Join(home, "old-config")); err != nil {
		t.Fatal(err)
	}
	write("replacement")
	wait("replacement")
	write("replacement-edited")
	wait("replacement-edited")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
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
