package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
)

func TestAcquireSingletonTakesFreeLock(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	// The lock is still held here, and Windows byte-range locks deny readers
	// where POSIX advisory locks do not. What the read is checking — that the
	// file records the holder's pid — is only observable off Windows.
	if runtime.GOOS == "windows" {
		t.Skip("a held byte-range lock is unreadable on Windows")
	}
	data, err := os.ReadFile(filepath.Join(dir, singletonLockName))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("lock file records pid %q, want %d", got, os.Getpid())
	}
}

// TestAcquireSingletonBlocksSecondHolder is the regression this exists for: a
// second server on the same data directory must not start. Before the lock it
// bound the unix socket straight over the incumbent and both kept running.
func TestAcquireSingletonBlocksSecondHolder(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	// The lock is per open file description, so a second acquire in this
	// process exercises the same kernel path a second process takes.
	if _, err := filelock.TryAcquire(filepath.Join(dir, singletonLockName)); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second acquire err = %v, want filelock.ErrBusy", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, err := acquireSingleton(ctx, dir, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting acquire err = %v, want DeadlineExceeded", err)
	}
}

// TestAcquireSingletonProceedsAfterRelease covers the handoff: the incumbent
// exiting must let the waiter in, which is what makes a dev rebuild recover
// instead of leaving two servers.
func TestAcquireSingletonProceedsAfterRelease(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		release()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	second, err := acquireSingleton(ctx, dir, nil)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	second()
}

// TestAcquireSingletonSeparateDataDirsDoNotConflict pins the chosen boundary:
// the lock is scoped to the data directory, so unrelated servers on their own
// databases are independent.
func TestAcquireSingletonSeparateDataDirsDoNotConflict(t *testing.T) {
	first, err := acquireSingleton(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first()
	second, err := acquireSingleton(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second()
}

func TestAcquireSingletonRequiresDataDir(t *testing.T) {
	if _, err := acquireSingleton(context.Background(), "  ", nil); err == nil {
		t.Fatal("acquire with empty data dir succeeded, want error")
	}
}

func TestDescribeSingletonHolderFallsBackWithoutPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, singletonLockName)
	if got := describeSingletonHolder(path); !strings.Contains(got, "another discobox-server") {
		t.Fatalf("missing lock file described as %q", got)
	}
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if got := describeSingletonHolder(path); got != "another discobox-server" {
		t.Fatalf("unparsable pid described as %q", got)
	}
	if err := os.WriteFile(path, []byte("4321\n"), 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if got := describeSingletonHolder(path); !strings.Contains(got, "4321") {
		t.Fatalf("described holder %q, want it to name pid 4321", got)
	}
}

func TestSingletonRecordLifetimeAndUnreachableDiagnostic(t *testing.T) {
	dir := t.TempDir()
	raw := "http://127.0.0.1:1"
	release, err := acquireSingleton(context.Background(), dir, []string{raw})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	record := filepath.Join(dir, "server-holder.json")
	info, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %v", info.Mode())
	}
	start := time.Now()
	_, err = acquireSingleton(context.Background(), dir, []string{"http://127.0.0.1:2"})
	if err == nil {
		t.Fatal("unreachable holder accepted")
	}
	for _, want := range []string{"unreachable", strconv.Itoa(os.Getpid()), raw, "--server", "server.lock", "127.0.0.1:2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q missing from %v", want, err)
		}
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("discovery exceeded initial answer budget")
	}
	release()
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("record survived release: %v", err)
	}
}

func TestSingletonPreservesStartupAndHandoffs(t *testing.T) {
	for _, mode := range []string{"starting", "accepted shutdown", "older upgrade"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			// A legacy holder supplies no metadata; the upgrade evidence comes from
			// the CLI that observed and stopped it before the listener disappeared.
			held, err := filelock.TryAcquire(filepath.Join(dir, singletonLockName))
			if err != nil {
				t.Fatal(err)
			}
			defer held.Release()
			raw := "http://127.0.0.1:1"
			if mode == "older upgrade" {
				t.Setenv("DISCOBOX_SERVER_HANDOFF", raw)
			} else {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "accepted shutdown" && r.URL.Path == "/shutdown" {
						w.WriteHeader(200)
						return
					}
					w.WriteHeader(503)
					_, _ = w.Write([]byte(`{"status":"starting","phase":"initializing providers"}`))
				}))
				defer server.Close()
				raw = server.URL
			}
			done := make(chan struct{})
			go func() { defer close(done); time.Sleep(4 * time.Second); _ = held.Release() }()
			defer func() { <-done }()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			release, err := acquireSingleton(ctx, dir, []string{raw})
			if err != nil {
				t.Fatalf("%s handoff: %v", mode, err)
			}
			release()
		})
	}
}

func TestSingletonStaleRecordDoesNotStrandFreeLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server-holder.json"), []byte(`{"pid":123,"endpoints":["http://old"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := acquireSingleton(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestSingletonStartingHealthSurvivesStalledShutdown(t *testing.T) {
	dir := t.TempDir()
	held, err := filelock.TryAcquire(filepath.Join(dir, singletonLockName))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shutdown" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"status":"starting","phase":"initializing"}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err = acquireSingleton(ctx, dir, []string{server.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("starting server misclassified: %v", err)
	}
}
