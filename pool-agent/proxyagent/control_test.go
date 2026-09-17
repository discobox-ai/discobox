package proxyagent

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/proxy"
)

func TestPrepareControlKeyIsCreatedOnceAndKept(t *testing.T) {
	withTestRoot(t)
	first, err := PrepareControlKey(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("first PrepareControlKey() error = %v", err)
	}
	second, err := PrepareControlKey(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("second PrepareControlKey() error = %v", err)
	}
	if !first.Equal(second) {
		t.Fatal("a second call made a new key; the proxy would trust one and the agent sign with the other")
	}
	info, err := os.Stat(resolve(layout.ProxyControlKey(testProjectID, testPoolID)))
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("key mode = %o, want 600", mode)
	}
}

// The pool agent and the proxy unit can both be first. Whoever loses must end
// up with the winner's key, not a key of its own.
func TestPrepareControlKeyConcurrentCallersAgree(t *testing.T) {
	withTestRoot(t)
	const callers = 16
	keys := make([]ed25519.PrivateKey, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = PrepareControlKey(testProjectID, testPoolID)
		}()
	}
	wg.Wait()
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if !keys[i].Equal(keys[0]) {
			t.Fatalf("caller %d holds a different key", i)
		}
	}
}

// The listener and the trust key come from one key, together: a control
// listener with no trust key is served unauthenticated.
func TestControlConfigTrustsTheKeysPublicHalf(t *testing.T) {
	withTestRoot(t)
	key, err := PrepareControlKey(testProjectID, testPoolID)
	if err != nil {
		t.Fatalf("PrepareControlKey() error = %v", err)
	}
	cfg, err := controlConfig(testProjectID, testPoolID, key)
	if err != nil {
		t.Fatalf("controlConfig() error = %v", err)
	}
	if cfg.ListenAddress != ControlListenAddress || cfg.ProjectID != testProjectID || cfg.WorkerID != testPoolID {
		t.Fatalf("control config = %+v", cfg)
	}
	publicKey, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("key has no ed25519 public half")
	}
	want := base64.StdEncoding.EncodeToString(publicKey)
	if cfg.TrustPublicKey != want {
		t.Fatalf("TrustPublicKey = %q, want the key's public half", cfg.TrustPublicKey)
	}
}

// A key file that exists but cannot be used must not strand the pool. It lives
// in the host state tree, so recreating the container brings the same file
// back; the agent replaces it at startup the way the CA code replaces a CA it
// cannot load.
func TestPrepareControlKeyReplacesAnUnusableKey(t *testing.T) {
	for name, content := range map[string]string{
		"empty (a crash between create and write)": "",
		"not base64":   "not a key at all\n",
		"wrong length": base64.StdEncoding.EncodeToString([]byte("short")) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			withTestRoot(t)
			path := resolve(layout.ProxyControlKey(testProjectID, testPoolID))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadControlKey(testProjectID, testPoolID); err == nil {
				t.Fatal("ReadControlKey accepted an unusable key")
			}
			key, err := PrepareControlKey(testProjectID, testPoolID)
			if err != nil {
				t.Fatalf("PrepareControlKey() over an unusable key = %v, want it replaced", err)
			}
			read, err := ReadControlKey(testProjectID, testPoolID)
			if err != nil || !read.Equal(key) {
				t.Fatalf("ReadControlKey() after repair = %v, want the replacement key", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("replacement key mode = %v, %v; want 600", info.Mode().Perm(), err)
			}
		})
	}
}

// Only the agent's startup creates or repairs the key. A reader that created
// one would race the agent, and the proxy could end up trusting a key the agent
// never signs with.
func TestReadControlKeyNeverCreatesOne(t *testing.T) {
	withTestRoot(t)
	if _, err := ReadControlKey(testProjectID, testPoolID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadControlKey() with no key = %v, want not-exist", err)
	}
	if _, err := os.Stat(resolve(layout.ProxyControlKey(testProjectID, testPoolID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadControlKey created a key file: %v", err)
	}
}

// Without a usable key the proxy runs with no control API — no listener and no
// trust key, never one without the other — and keeps serving sandbox traffic.
func TestProxyControlConfigIsOffWithoutAUsableKey(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	withTestRoot(t)
	path := resolve(layout.ProxyControlKey(testProjectID, testPoolID))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg := proxyControlConfig(testProjectID, testPoolID, logger); cfg != (proxy.ControlConfig{}) {
		t.Fatalf("control config with an unusable key = %+v, want it off entirely", cfg)
	}

	if _, err := PrepareControlKey(testProjectID, testPoolID); err != nil {
		t.Fatal(err)
	}
	cfg := proxyControlConfig(testProjectID, testPoolID, logger)
	if cfg.ListenAddress != ControlListenAddress || cfg.TrustPublicKey == "" {
		t.Fatalf("control config with a usable key = %+v, want the listener and its trust key", cfg)
	}
}
