package proxyagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/fsdurable"
	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/proxy"
)

// The pool proxy's control API, and who may read it (ADR 0130 §4).
//
// The control plane never talks to the proxy. It asks the pool agent, and the
// pool agent relays over loopback with a token it signs itself, so the key the
// proxy trusts never leaves the pool.

// ControlListenAddress is where the pool proxy serves its read-only control API.
// Loopback, unlike ListenAddress: the pool agent shares the pool's network
// namespace with the proxy unit, and nothing a sandbox runs can reach it.
const ControlListenAddress = "127.0.0.1:17084"

const controlBaseURL = "http://" + ControlListenAddress

// PrepareControlKey makes sure the pool has a usable proxy control key and
// returns it. Only the pool agent calls it, at startup and before systemd
// starts the proxy unit, the same point the CA is prepared at: everything that
// reads the key afterwards (the proxy unit, the audit relay) calls
// ReadControlKey and never writes.
//
// A missing key is created. A key that is present but unusable — empty, not
// base64, the wrong length — is replaced, the way PrepareCertificates replaces
// a CA it cannot load. Failing instead would strand the pool: the key lives in
// the host state tree, so recreating the pool container brings the same file
// back, and a pool that cannot start its proxy has no sandbox egress at all.
// Replacing the key costs nothing but control tokens signed with the old one,
// which live minutes and are only ever signed by this process.
//
// A new key is written to a temporary file and synced before it is linked into
// place, and the directory is synced after, so a crash leaves either no key or
// a whole one, never an empty file at the final path.
func PrepareControlKey(projectID, poolID string) (ed25519.PrivateKey, error) {
	path := resolve(layout.ProxyControlKey(projectID, poolID))
	key, err := readControlKey(path)
	switch {
	case err == nil:
		return key, nil
	case errors.Is(err, os.ErrNotExist):
		return createControlKey(path, false)
	case errors.Is(err, errUnusableControlKey):
		return createControlKey(path, true)
	default:
		return nil, err
	}
}

// ReadControlKey returns the pool's proxy control key without ever writing it.
// An error means the key is missing or unusable; the caller runs without the
// control API rather than failing, because an audit read must not be able to
// break sandbox egress.
func ReadControlKey(projectID, poolID string) (ed25519.PrivateKey, error) {
	return readControlKey(resolve(layout.ProxyControlKey(projectID, poolID)))
}

var errUnusableControlKey = errors.New("proxy control key is unusable")

// createControlKey writes a fresh key to path. Without replace it links the key
// into place and, if another writer got there first, returns theirs, so two
// first boots racing cannot leave the proxy trusting one key and the agent
// signing with another. With replace it renames over the unusable file.
func createControlKey(path string, replace bool) (ed25519.PrivateKey, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".control-key-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(key.Seed()) + "\n"); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if replace {
		if err := os.Rename(tmp.Name(), path); err != nil {
			return nil, err
		}
	} else if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	if err := fsdurable.SyncDir(dir); err != nil {
		return nil, err
	}
	return readControlKey(path)
}

func readControlKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errUnusableControlKey, path, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: %s holds %d bytes, want %d", errUnusableControlKey, path, len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// proxyControlConfig is the Control config the proxy unit runs with. When the
// key cannot be read it is the zero config — no listener, no trust key — and
// the proxy serves sandbox traffic without its control API. Losing the audit
// read is the smaller failure; losing egress for every sandbox on the pool is
// not one an audit read should be able to cause.
func proxyControlConfig(projectID, poolID string, logger *slog.Logger) proxy.ControlConfig {
	key, err := ReadControlKey(projectID, poolID)
	if err == nil {
		var cfg proxy.ControlConfig
		if cfg, err = controlConfig(projectID, poolID, key); err == nil {
			return cfg
		}
	}
	logger.Warn("pool proxy control API disabled: its key cannot be used", "error", err)
	return proxy.ControlConfig{}
}

// controlConfig is the proxy's Control config for this pool. The listener and
// the trust key are set together from one key, so the configuration ADR 0130
// §4 forbids — a listener with no key, which the proxy serves unauthenticated —
// cannot be written here.
func controlConfig(projectID, poolID string, key ed25519.PrivateKey) (proxy.ControlConfig, error) {
	publicKey, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return proxy.ControlConfig{}, errors.New("proxy control key has no ed25519 public half")
	}
	return proxy.ControlConfig{
		ListenAddress:  ControlListenAddress,
		TrustPublicKey: base64.StdEncoding.EncodeToString(publicKey),
		ProjectID:      projectID,
		WorkerID:       poolID,
	}, nil
}

// NewAuditClient returns a client for this pool's proxy control API, signing
// with the pool's control key. It only reads the key; PrepareControlKey, run at
// agent startup, is what creates or repairs it.
func NewAuditClient(projectID, poolID string) (*proxy.ControlClient, error) {
	key, err := ReadControlKey(projectID, poolID)
	if err != nil {
		return nil, fmt.Errorf("read proxy control key: %w", err)
	}
	return proxy.NewControlClient(controlBaseURL, key, projectID, poolID, nil), nil
}
