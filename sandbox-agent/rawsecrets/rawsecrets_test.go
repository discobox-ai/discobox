package rawsecrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

func testHolder(t *testing.T, now func() time.Time) *Holder {
	t.Helper()
	return New(Config{
		Dir:      filepath.Join(t.TempDir(), "raw-secrets"),
		OnMemory: func(string) error { return nil },
		Now:      now,
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// answer tells h the set secrets name and applies them as the pool's answer
// for it.
func answer(h *Holder, secrets ...sandboxconfig.RawSecret) error {
	set := map[string]string{}
	for _, secret := range secrets {
		set[secret.Name] = secret.Delivery
	}
	h.SetApplied(set)
	h.mu.RLock()
	gen := h.gen
	h.mu.RUnlock()
	return h.apply(sandboxconfig.RawSecretsResponse{Secrets: secrets}, gen)
}

func TestApplyKeepsEachValueInItsOwnPrivateFile(t *testing.T) {
	h := testHolder(t, nil)
	if err := answer(h,
		sandboxconfig.RawSecret{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryEnv, Value: "key-1"},
		sandboxconfig.RawSecret{Name: "SIGNING_KEY", Delivery: sandboxconfig.RawDeliveryFile, Value: "pem"},
	); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.Path("API_KEY")); got != "key-1" {
		t.Fatalf("API_KEY = %q", got)
	}
	if got := h.Deliveries(); got["API_KEY"] != sandboxconfig.RawDeliveryEnv || got["SIGNING_KEY"] != sandboxconfig.RawDeliveryFile || len(got) != 2 {
		t.Fatalf("deliveries = %v", got)
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Stat(h.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if dir.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %v, want 0700", dir.Mode().Perm())
		}
		file, err := os.Stat(h.Path("API_KEY"))
		if err != nil {
			t.Fatal(err)
		}
		if file.Mode().Perm() != 0o400 {
			t.Fatalf("file mode = %v, want 0400", file.Mode().Perm())
		}
	}

	// A rotated value replaces the file; one no longer held is removed.
	if err := answer(h, sandboxconfig.RawSecret{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryEnv, Value: "key-2"}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.Path("API_KEY")); got != "key-2" {
		t.Fatalf("rotated API_KEY = %q", got)
	}
	if _, err := os.Stat(h.Path("SIGNING_KEY")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed secret's file still there: %v", err)
	}
	entries, err := os.ReadDir(h.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only API_KEY's file", len(entries))
	}
}

func TestApplyDropsAValueTheAppliedDocumentDoesNotName(t *testing.T) {
	h := testHolder(t, nil)
	h.SetApplied(map[string]string{"API_KEY": sandboxconfig.RawDeliveryFile})
	err := h.apply(sandboxconfig.RawSecretsResponse{Secrets: []sandboxconfig.RawSecret{
		{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryFile, Value: "a"},
		{Name: "EXTRA", Delivery: sandboxconfig.RawDeliveryFile, Value: "b"},
		{Name: "API_KEY2", Delivery: sandboxconfig.RawDeliveryEnv, Value: "c"},
	}}, h.gen)
	if err == nil {
		t.Fatal("values the document does not name were not reported")
	}
	if got := h.Deliveries(); len(got) != 1 || got["API_KEY"] != sandboxconfig.RawDeliveryFile {
		t.Fatalf("deliveries = %v, want API_KEY alone", got)
	}
	if _, err := os.Stat(h.Path("EXTRA")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a value the document does not name was written: %v", err)
	}
}

func TestApplyRefusesADirectoryNotInMemory(t *testing.T) {
	h := New(Config{
		Dir:      filepath.Join(t.TempDir(), "raw-secrets"),
		OnMemory: func(string) error { return errors.New("on disk") },
	})
	if err := answer(h, sandboxconfig.RawSecret{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryEnv, Value: "key"}); err == nil {
		t.Fatal("a value was written to a directory not in memory")
	}
	if _, err := os.Stat(h.Path("API_KEY")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("value written: %v", err)
	}
	if len(h.Deliveries()) != 0 {
		t.Fatal("a value not kept is reported held")
	}
}

func TestApplySkipsAnInvalidEntry(t *testing.T) {
	h := testHolder(t, nil)
	err := answer(h,
		sandboxconfig.RawSecret{Name: "../escape", Delivery: sandboxconfig.RawDeliveryEnv, Value: "x"},
		sandboxconfig.RawSecret{Name: "GOOD", Delivery: "both", Value: "x"},
		sandboxconfig.RawSecret{Name: "OK", Delivery: sandboxconfig.RawDeliveryFile, Value: "y"},
	)
	if err == nil {
		t.Fatal("invalid entries were not reported")
	}
	if got := h.Deliveries(); len(got) != 1 || got["OK"] != sandboxconfig.RawDeliveryFile {
		t.Fatalf("deliveries = %v, want OK alone", got)
	}
}

func TestAnEmptySetRemovesEverythingHeld(t *testing.T) {
	h := testHolder(t, nil)
	if err := answer(h, sandboxconfig.RawSecret{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryFile, Value: "v"}); err != nil {
		t.Fatal(err)
	}
	h.SetApplied(nil)
	if len(h.Deliveries()) != 0 {
		t.Fatal("an empty set left values held")
	}
	if _, err := os.Stat(h.Path("API_KEY")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file kept under an empty set: %v", err)
	}
}

func TestSweepRemovesWhatAPreviousRunKept(t *testing.T) {
	h := testHolder(t, nil)
	if err := os.MkdirAll(h.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"OLD", ".OLD.tmp"} {
		if err := os.WriteFile(filepath.Join(h.Dir(), name), []byte("stale"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Sweep(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(h.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("sweep left %d entries", len(entries))
	}
	if err := New(Config{Dir: filepath.Join(t.TempDir(), "absent")}).Sweep(); err != nil {
		t.Fatalf("sweep of an absent directory: %v", err)
	}
}

func TestExpireKeepsWhatIsHeldThroughAFailedAskUntilItExpires(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	h := testHolder(t, clock)
	expires := now.Add(time.Minute)
	if err := answer(h,
		sandboxconfig.RawSecret{Name: "LASTING", Delivery: sandboxconfig.RawDeliveryEnv, Value: "a"},
		sandboxconfig.RawSecret{Name: "EXPIRING", Delivery: sandboxconfig.RawDeliveryEnv, Value: "b", ExpiresAt: &expires},
	); err != nil {
		t.Fatal(err)
	}
	h.expire()
	if len(h.Deliveries()) != 2 {
		t.Fatal("a failed ask dropped values that had not expired")
	}
	mu.Lock()
	now = expires
	mu.Unlock()
	h.expire()
	if got := h.Deliveries(); len(got) != 1 || got["LASTING"] == "" {
		t.Fatalf("deliveries after expiry = %v, want LASTING alone", got)
	}
	if _, err := os.Stat(h.Path("EXPIRING")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired value's file still there: %v", err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunAsksOnlyWhenToldAndRemovesEverythingWhenItEnds(t *testing.T) {
	h := testHolder(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	var asks atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx, func(context.Context) (sandboxconfig.RawSecretsResponse, error) {
			asks.Add(1)
			return sandboxconfig.RawSecretsResponse{Secrets: []sandboxconfig.RawSecret{
				{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryFile, Value: "v"},
			}}, nil
		})
	}()
	// Nothing applied, then an empty set: nothing to ask for.
	time.Sleep(50 * time.Millisecond)
	h.SetApplied(map[string]string{})
	time.Sleep(50 * time.Millisecond)
	if asks.Load() != 0 {
		t.Fatalf("asked %d times with no raw secrets applied", asks.Load())
	}
	h.SetApplied(map[string]string{"API_KEY": sandboxconfig.RawDeliveryFile})
	waitFor(t, "the value", func() bool { return len(h.Deliveries()) == 1 })
	if got := readFile(t, h.Path("API_KEY")); got != "v" {
		t.Fatalf("API_KEY = %q", got)
	}
	cancel()
	<-done
	if _, err := os.Stat(h.Path("API_KEY")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("value kept after Run ended: %v", err)
	}
}

func TestAwaitFetchedHoldsUntilThePoolAnswersForTheAppliedSet(t *testing.T) {
	h := testHolder(t, nil)
	if err := h.AwaitFetched(context.Background()); err != nil {
		t.Fatalf("await with nothing applied: %v", err)
	}
	h.SetApplied(map[string]string{"API_KEY": sandboxconfig.RawDeliveryEnv})
	returned := make(chan struct{})
	go func() {
		_ = h.AwaitFetched(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("await returned before the pool answered")
	case <-time.After(50 * time.Millisecond):
	}
	h.mu.RLock()
	gen := h.gen
	h.mu.RUnlock()
	if err := h.apply(sandboxconfig.RawSecretsResponse{Secrets: []sandboxconfig.RawSecret{
		{Name: "API_KEY", Delivery: sandboxconfig.RawDeliveryEnv, Value: "v"},
	}}, gen); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("await did not return once the pool answered")
	}
	// An answer for a set since replaced settles nothing.
	h.SetApplied(map[string]string{"OTHER": sandboxconfig.RawDeliveryEnv})
	_ = h.apply(sandboxconfig.RawSecretsResponse{}, gen)
	h.mu.RLock()
	settled := h.fetchedGen == h.gen
	h.mu.RUnlock()
	if settled {
		t.Fatal("an answer for a replaced set settled the new one")
	}
}

func TestAwaitFetchedIsBounded(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	h := testHolder(t, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	h.SetApplied(map[string]string{"API_KEY": sandboxconfig.RawDeliveryEnv})
	mu.Lock()
	now = now.Add(AwaitBound)
	mu.Unlock()
	start := time.Now()
	if err := h.AwaitFetched(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("await past its bound waited again")
	}
}

func TestUntilRenewalAsksBeforeAValueExpires(t *testing.T) {
	now := time.Now()
	h := testHolder(t, func() time.Time { return now })
	soon := now.Add(expirySkew + 2*time.Second)
	if err := answer(h, sandboxconfig.RawSecret{Name: "TOKEN", Delivery: sandboxconfig.RawDeliveryEnv, Value: "t", ExpiresAt: &soon}); err != nil {
		t.Fatal(err)
	}
	if got := h.untilRenewal(); got != 2*time.Second {
		t.Fatalf("untilRenewal = %v, want 2s", got)
	}
}
