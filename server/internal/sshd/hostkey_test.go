package sshd

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/internal/privatefs"
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

	// Whatever "only this user" means on the platform running the test: a mode
	// on Unix, an access list on Windows, which the mode would not have set.
	if err := privatefs.Check(filepath.Join(dir, hostKeyFileName)); err != nil {
		t.Fatal(err)
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
