package poolauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A pool's identity key is kept in a file on the pool's own durable storage
// (layout.Root.PoolIdentityKey, ADR 0063): the standard base64 of the Ed25519
// private key, root's alone. It is made on the pool, the first time its maker
// asks for its public half (ADR 26-10-24-953 §4), and never leaves it.

// LoadKey reads the identity key at path. A key that is not there is an
// fs.ErrNotExist; one that is there and unreadable is never replaced.
func LoadKey(path string) (ed25519.PrivateKey, error) {
	encoded, err := os.ReadFile(path) //nolint:gosec // G304: the path is the pool's identity key file.
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		return nil, fmt.Errorf("decode pool identity key %s: %w", path, err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("pool identity key %s has length %d, want %d", path, len(decoded), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(decoded), nil
}

// LoadOrCreateKey reads the identity key at path, making it there first when
// none is, and reports whether it made it now. A key that is there and cannot
// be read is an error, never replaced: a new one would be another pool.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, bool, error) {
	key, err := LoadKey(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return key, false, err
	}
	_, key, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("generate pool identity key: %w", err)
	}
	if err := storeKey(path, key); err != nil {
		return nil, false, err
	}
	return key, true, nil
}

// PublicKeyOf is key's public half, encoded as the control plane records it
// (EncodePublicKey).
func PublicKeyOf(key ed25519.PrivateKey) (string, error) {
	public, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return "", errors.New("pool identity key is not ed25519")
	}
	return EncodePublicKey(public)
}

// storeKey writes key through a temporary file in the same directory, so an
// interrupted write never leaves a truncated key that the next read refuses.
func storeKey(path string, key ed25519.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create pool identity directory: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".agent.key-")
	if err != nil {
		return fmt.Errorf("create pool identity key: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure pool identity key: %w", err)
	}
	if _, err := file.WriteString(base64.StdEncoding.EncodeToString(key)); err != nil {
		_ = file.Close()
		return fmt.Errorf("write pool identity key: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write pool identity key: %w", err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return fmt.Errorf("publish pool identity key: %w", err)
	}
	return nil
}
