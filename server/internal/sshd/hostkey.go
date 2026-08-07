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

	// Write-then-link, not a direct O_EXCL create: O_CREATE|O_EXCL makes the
	// *creation* atomic, but the content write that follows it is not, so a
	// concurrent reader (another goroutine or process racing the same
	// startup) can open the file the instant it is created and read it
	// before the winner finishes writing, seeing a truncated key. Writing
	// the complete PEM to a temp file first and only then linking it into
	// place means the target's content is always complete by the time any
	// reader — including the loser of the race, immediately below — can see
	// it at all: Link either produces a fully-written file or fails with
	// ErrExist and touches nothing.
	tmpFile, err := os.CreateTemp(dataDir, hostKeyFileName+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create SSH host key temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath) // best-effort: harmless if Link already moved it out of the way conceptually (a hard link leaves the temp name too, so this always has something to remove).
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("chmod SSH host key temp file: %w", err)
	}
	if _, err := tmpFile.Write(pemBytes); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("write SSH host key: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("write SSH host key: %w", err)
	}

	if err := os.Link(tmpPath, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("write SSH host key: %w", err)
		}
		// Reload the winner's bytes rather than trust our own, mirroring the
		// "reload the winner" idiom poolagent/auth.go's EnsureTrustKey uses
		// for a DB unique-constraint race, adapted here to a filesystem one.
		existing, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read SSH host key after generation race: %w", err)
		}
		return ssh.ParsePrivateKey(existing)
	}
	return ssh.ParsePrivateKey(pemBytes)
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
