package sshd

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestLoadOrCreateHostKeyGeneratesOnce(t *testing.T) {
	dir := t.TempDir()

	signer1, err := LoadOrCreateHostKey(dir)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	signer2, err := LoadOrCreateHostKey(dir)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if string(signer1.PublicKey().Marshal()) != string(signer2.PublicKey().Marshal()) {
		t.Fatalf("expected the same key across calls")
	}

	info, err := os.Stat(filepath.Join(dir, hostKeyFileName))
	if err != nil {
		t.Fatalf("stat host key file: %v", err)
	}
	// Windows reports 0666 for any writable file whatever its ACL, so there is
	// no mode here to assert. The gap is real and named rather than hidden:
	// nothing ACL-restricts the host key on Windows, the way the CLI's
	// restrictToUser does for its state files.
	if runtime.GOOS == "windows" {
		t.Skip("no Unix mode on Windows; the host key is not ACL-restricted there either")
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestLoadOrCreateHostKeyConcurrentRaceConverges(t *testing.T) {
	dir := t.TempDir()

	const n = 8
	var wg sync.WaitGroup
	keys := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			signer, err := LoadOrCreateHostKey(dir)
			errs[i] = err
			if err == nil {
				keys[i] = string(signer.PublicKey().Marshal())
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if keys[i] != keys[0] {
			t.Fatalf("goroutine %d got a different key than goroutine 0", i)
		}
	}
}

func TestLoadOrCreateHostKeyRequiresDataDir(t *testing.T) {
	if _, err := LoadOrCreateHostKey(""); err == nil {
		t.Fatalf("expected an error for an empty data directory")
	}
}
