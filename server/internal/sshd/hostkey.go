package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// hostKeyFileName is the persistent SSH host key inside the server's data
// directory. It is written once and never regenerated implicitly: rotating it
// would break every enrolled client's known_hosts entry (ADR 0024
// consequences).
const hostKeyFileName = "ssh_host_ed25519_key"

// LoadOrCreateHostKey returns the server's persistent SSH host key from
// <dataDir>/ssh_host_ed25519_key, generating and writing it once if absent.
func LoadOrCreateHostKey(dataDir string) (ssh.Signer, error) {
	if dataDir == "" {
		return nil, errors.New("data directory is required for the SSH host key")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, hostKeyFileName)

	if pemBytes, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(pemBytes)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read SSH host key: %w", err)
	}

	pemBytes, err := generateHostKeyPEM()
	if err != nil {
		return nil, err
	}

	// O_EXCL makes the create atomic: if another process (or another goroutine
	// racing the same startup) wins, this open fails and the loser must load
	// the winner's bytes rather than overwrite them — the host key must never
	// be regenerated once set. Mirrors the "reload the winner" idiom
	// poolagent/auth.go's EnsureTrustKey uses for a DB unique-constraint race,
	// adapted here to a filesystem race.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		_, writeErr := file.Write(pemBytes)
		closeErr := file.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("write SSH host key: %w", writeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("write SSH host key: %w", closeErr)
		}
		return ssh.ParsePrivateKey(pemBytes)
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("write SSH host key: %w", err)
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SSH host key after generation race: %w", err)
	}
	return ssh.ParsePrivateKey(existing)
}

func generateHostKeyPEM() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal SSH host key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
