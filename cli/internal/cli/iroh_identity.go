package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/obot-platform/discobox/endpoint"
)

// irohIdentityFileName is this machine's iroh identity: the key whose public
// half an operator enrolls in a server's authorized_ids.
//
// It lives under the CLI's own state directory rather than ~/.ssh because
// nothing outside discobox reads it — unlike the SSH identity, which exists so
// the `ssh` binary can use it.
const irohIdentityFileName = "id_ed25519"

func defaultIrohIdentityPath() string {
	return filepath.Join(cliStateDir(), "iroh", irohIdentityFileName)
}

// loadOrCreateIrohIdentity returns the endpoint ID at path, generating an
// ed25519 keypair there when it is absent. It reports whether it generated one
// so the caller can say so rather than silently creating a credential.
func loadOrCreateIrohIdentity(path string) (id endpoint.IrohID, created bool, err error) {
	if existing, err := readIrohIdentity(path); err == nil {
		return existing, false, nil
	} else if !os.IsNotExist(err) {
		return endpoint.IrohID{}, false, err
	}

	if err := ensureStateDir(filepath.Dir(path)); err != nil {
		return endpoint.IrohID{}, false, fmt.Errorf("create iroh identity directory: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return endpoint.IrohID{}, false, fmt.Errorf("generate iroh identity: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return endpoint.IrohID{}, false, fmt.Errorf("marshal iroh identity: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return endpoint.IrohID{}, false, fmt.Errorf("write iroh identity: %w", err)
	}
	if err := restrictToUser(path); err != nil {
		return endpoint.IrohID{}, false, fmt.Errorf("restrict iroh identity to this user: %w", err)
	}
	newID, err := endpoint.IrohIDFromPublicKey(pub)
	if err != nil {
		return endpoint.IrohID{}, false, err
	}
	return newID, true, nil
}

// readIrohIdentity returns the endpoint ID of the key at path. A missing file
// is reported as os.ErrNotExist so the caller can distinguish "no identity yet"
// from "the identity is broken".
func readIrohIdentity(path string) (endpoint.IrohID, error) {
	key, err := readIrohIdentityKey(path)
	if err != nil {
		return endpoint.IrohID{}, err
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return endpoint.IrohID{}, errors.New("iroh identity is not ed25519")
	}
	return endpoint.IrohIDFromPublicKey(pub)
}

// configureIrohForEndpoint installs this machine's iroh identity when the
// server endpoint needs one.
//
// It runs only for an iroh endpoint: generating a key and opening a UDP socket
// is not something `disco ls` against a unix socket should do. The identity is
// the one `disco box iroh-id` prints, so what an operator enrolled is what
// connects.
func configureIrohForEndpoint(parsed endpoint.Endpoint) error {
	if parsed.Scheme != "iroh" {
		return nil
	}
	path := defaultIrohIdentityPath()
	key, err := loadOrCreateIrohIdentityKey(path)
	if err != nil {
		return err
	}
	if err := endpoint.ConfigureIroh(endpoint.IrohConfig{SecretKey: key}); err != nil {
		if strings.Contains(err.Error(), errIrohAlreadyConfigured.Error()) {
			return nil
		}
		return err
	}
	return nil
}

// errIrohAlreadyConfigured matches the endpoint package's message for a second
// ConfigureIroh call. A CLI process can build more than one client — the git
// loopback proxy builds its own — and the second one wants the identity that is
// already installed, not an error.
var errIrohAlreadyConfigured = errors.New("iroh is already configured for this process")

// loadOrCreateIrohIdentityKey returns the secret key at path, generating it if
// absent. It is the key half of loadOrCreateIrohIdentity, which returns only
// the public ID.
func loadOrCreateIrohIdentityKey(path string) (ed25519.PrivateKey, error) {
	if key, err := readIrohIdentityKey(path); err == nil {
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, _, err := loadOrCreateIrohIdentity(path); err != nil {
		return nil, err
	}
	return readIrohIdentityKey(path)
}

func readIrohIdentityKey(path string) (ed25519.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("iroh identity %s is not PEM", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse iroh identity %s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("iroh identity %s is %T, want ed25519", path, parsed)
	}
	return key, nil
}
