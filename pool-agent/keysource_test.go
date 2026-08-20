package poolagent

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

// The whole point of persisting the key: an agent that restarts is still the
// same pool, so it needs no bootstrap token to say so.
func TestFileKeySourceReusesTheSameIdentityAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "agent.key")
	source := FileKeySource{Path: path}

	first, err := source.KeyPair(context.Background())
	if err != nil {
		t.Fatalf("first KeyPair: %v", err)
	}
	if first.Loaded {
		t.Error("a freshly generated key reported itself as loaded")
	}
	if len(first.PrivateKey) != ed25519.PrivateKeySize {
		t.Fatalf("private key length = %d", len(first.PrivateKey))
	}

	second, err := source.KeyPair(context.Background())
	if err != nil {
		t.Fatalf("second KeyPair: %v", err)
	}
	if !second.Loaded {
		t.Fatal("the second start generated a new key instead of loading the stored one")
	}
	if second.PublicKey != first.PublicKey {
		t.Errorf("public key changed across restarts: %q then %q", first.PublicKey, second.PublicKey)
	}
	if !second.PrivateKey.Equal(first.PrivateKey) {
		t.Error("private key changed across restarts")
	}
}

// The key authenticates as the pool, so it must not be readable by anything
// else sharing the host.
func TestFileKeySourceWritesThePrivateKeyUnreadableToOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "agent.key")
	if _, err := (FileKeySource{Path: path}).KeyPair(context.Background()); err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key mode = %o, want 600", perm)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("identity directory mode = %o, want no group or other access", perm)
	}
}

// A key that exists but cannot be read must fail loudly. Generating a new one
// would silently change the pool's identity and then need a bootstrap token
// that was already spent.
func TestFileKeySourceRefusesAnUnreadableKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	if err := os.WriteFile(path, []byte("not base64 at all !!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileKeySource{Path: path}).KeyPair(context.Background()); err == nil {
		t.Fatal("KeyPair silently replaced a corrupt identity key")
	}
}

func TestFileKeySourceRequiresAPath(t *testing.T) {
	if _, err := (FileKeySource{}).KeyPair(context.Background()); err == nil {
		t.Fatal("KeyPair accepted an empty path")
	}
}

// countingRegistrar records how many times a pool registered.
type countingRegistrar struct {
	calls      int
	publicKeys []string
	err        error
}

func (c *countingRegistrar) RegisterPool(_ context.Context, req RegisterRequest) (*RegisterResponse, error) {
	c.calls++
	c.publicKeys = append(c.publicKeys, req.PublicKey)
	if c.err != nil {
		return nil, c.err
	}
	return &RegisterResponse{}, nil
}

func testBootstrap() Bootstrap {
	return Bootstrap{
		ControlPlaneURL: "http://control-plane",
		ProjectID:       "proj-1",
		PoolID:          "pool-1",
		Token:           "bootstrap-token",
		ControlPlaneKey: "key",
	}
}

// The behavior this exists for: a bootstrap token is single-use, so an agent
// that already has an identity must not spend one on every restart.
func TestRunRegistersOnlyOnFirstContact(t *testing.T) {
	source := FileKeySource{Path: filepath.Join(t.TempDir(), "agent.key")}
	registrar := &countingRegistrar{}

	first, err := Run(context.Background(), Config{Bootstrap: testBootstrap(), Client: registrar, KeySource: source})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if registrar.calls != 1 {
		t.Fatalf("first start registered %d times, want 1", registrar.calls)
	}
	if first.FromStoredKey {
		t.Error("first start reported a stored key")
	}

	second, err := Run(context.Background(), Config{Bootstrap: testBootstrap(), Client: registrar, KeySource: source})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if registrar.calls != 1 {
		t.Fatalf("restart registered again (%d calls); its bootstrap token was already spent", registrar.calls)
	}
	if !second.FromStoredKey {
		t.Error("restart did not report a stored key")
	}
	if second.PublicKey != first.PublicKey {
		t.Error("restart changed the pool's identity")
	}
}

// ForceRegister is the recovery path for a control plane that has forgotten
// this key. It must republish the same identity rather than mint a new one.
func TestRunForceRegisterRepublishesTheStoredIdentity(t *testing.T) {
	source := FileKeySource{Path: filepath.Join(t.TempDir(), "agent.key")}
	registrar := &countingRegistrar{}

	first, err := Run(context.Background(), Config{Bootstrap: testBootstrap(), Client: registrar, KeySource: source})
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	forced, err := Run(context.Background(), Config{Bootstrap: testBootstrap(), Client: registrar, KeySource: source, ForceRegister: true})
	if err != nil {
		t.Fatalf("forced Run: %v", err)
	}
	if registrar.calls != 2 {
		t.Fatalf("forced start registered %d times, want 2", registrar.calls)
	}
	if forced.PublicKey != first.PublicKey {
		t.Error("forced registration changed the pool's identity")
	}
	if registrar.publicKeys[0] != registrar.publicKeys[1] {
		t.Error("forced registration published a different public key")
	}
}
