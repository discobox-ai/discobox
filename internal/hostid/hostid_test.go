package hostid

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adrg/xdg"

	"github.com/obot-platform/discobox/id"
)

// useTempConfigHome points XDG config resolution at a temp directory. xdg
// caches its directories at init, so the reload is required for the change to
// take effect.
func useTempConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv(EnvVar, "")
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	return dir
}

func TestGetIsStableAcrossCalls(t *testing.T) {
	useTempConfigHome(t)

	first, err := Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	second, err := Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if first != second {
		t.Fatalf("host ID changed between calls: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, id.PrefixHost+"_") || !id.IsGenerated(first) {
		t.Fatalf("host ID %q is not a generated %s ID", first, id.PrefixHost)
	}
}

func TestGetPersistsToConfigFile(t *testing.T) {
	dir := useTempConfigHome(t)

	generated, err := Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	path := filepath.Join(dir, appName, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read host ID file: %v", err)
	}
	if strings.TrimSpace(string(data)) != generated {
		t.Fatalf("host ID file holds %q, want %q", strings.TrimSpace(string(data)), generated)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The identity is not a secret, but it is per-user state and should not be
	// world-readable in a shared home.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("host ID file mode = %o, want 600", perm)
	}
}

func TestGetEnvOverrideWinsAndDoesNotPersist(t *testing.T) {
	dir := useTempConfigHome(t)
	t.Setenv(EnvVar, "host_overridevalue1")

	got, err := Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "host_overridevalue1" {
		t.Fatalf("host ID = %q, want the override", got)
	}
	// An override is for ephemeral environments; writing it back would make a
	// container's borrowed identity outlive the override.
	if _, err := os.Stat(filepath.Join(dir, appName, fileName)); !os.IsNotExist(err) {
		t.Fatalf("override wrote a host ID file: %v", err)
	}
}

// A truncated or hand-edited file must not wedge every command that needs an
// identity, so an unparseable value is treated as absent.
func TestGetRegeneratesCorruptFile(t *testing.T) {
	dir := useTempConfigHome(t)
	path := filepath.Join(dir, appName, fileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("host_trunc"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == "host_trunc" || !id.IsGenerated(got) {
		t.Fatalf("host ID = %q, want a regenerated ID", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != got {
		t.Fatalf("corrupt file was not replaced: holds %q, want %q", strings.TrimSpace(string(data)), got)
	}
}

// Two first runs racing in one config directory must settle on one identity,
// not mint separate ones and split a user's sandbox listings.
func TestGetConcurrentFirstRunAgreesOnOneIdentity(t *testing.T) {
	useTempConfigHome(t)

	const workers = 8
	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = Get()
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: Get: %v", i, err)
		}
	}
	for i, got := range results {
		if got != results[0] {
			t.Fatalf("worker %d returned %q, want %q from worker 0", i, got, results[0])
		}
	}
}
