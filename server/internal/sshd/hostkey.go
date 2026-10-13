package sshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/internal/privatefs"
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
		// Restricted again on every load, which is what repairs a key an
		// earlier Windows server wrote with nothing but the directory's ACL.
		// Best-effort: a key this server can read but not re-permission (owned
		// by another account, granted read-only, on a volume with no ACLs) is
		// no less exposed for the server refusing to start, so it is logged as
		// an error and the key is used as it is.
		if err := privatefs.Restrict(path); err != nil {
			slog.Error("could not restrict the SSH host key to this user; it keeps the permissions it has",
				"path", path, "error", err)
		}
		return ssh.ParsePrivateKey(pemBytes)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read SSH host key: %w", err)
	}

	pemBytes, err := generateHostKeyPEM()
	if err != nil {
		return nil, err
	}

	// Private from the moment it exists and complete from the moment it is
	// visible, and never replacing a key another process racing this startup
	// wrote first: see privatefs.WriteExclusive.
	if err := privatefs.WriteExclusive(path, pemBytes); err != nil {
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
