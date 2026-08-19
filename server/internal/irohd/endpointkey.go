// Package irohd owns the server's iroh identity and the endpoint IDs it will
// accept connections from (ADR 0052 §5).
//
// It is the iroh counterpart of internal/sshd's host key and authorized_keys
// handling, and for the same reasons: the identity is an address that must
// survive restarts, and the authorization layer must work before any API
// access exists.
package irohd

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/obot-platform/discobox/endpoint"
)

// endpointKeyFileName is the server's persistent iroh identity inside its data
// directory. It is written once and never regenerated implicitly: the public
// half of this key *is* the server's address, so rotating it invalidates every
// client's configured endpoint, the same property the SSH host key has.
const endpointKeyFileName = "iroh_endpoint_key"

// LoadOrCreateEndpointKey returns the server's persistent iroh secret key from
// <dataDir>/iroh_endpoint_key, generating and writing it once if absent.
func LoadOrCreateEndpointKey(dataDir string) (ed25519.PrivateKey, error) {
	if dataDir == "" {
		return nil, errors.New("data directory is required for the iroh endpoint key")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dataDir, endpointKeyFileName)

	if pemBytes, err := os.ReadFile(path); err == nil {
		return parseEndpointKey(pemBytes)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read iroh endpoint key: %w", err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate iroh endpoint key: %w", err)
	}
	pemBytes, err := marshalEndpointKey(priv)
	if err != nil {
		return nil, err
	}

	// Write-then-link rather than O_CREATE|O_EXCL, for the reason
	// sshd.LoadOrCreateHostKey documents at length: exclusive creation makes
	// only the creation atomic, so a process racing this startup can open the
	// file between create and write and read a truncated key. Linking a
	// fully-written temp file into place means any reader that can see the
	// path at all sees complete content.
	tmpFile, err := os.CreateTemp(dataDir, endpointKeyFileName+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create iroh endpoint key temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmpFile.Chmod(0o600); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("chmod iroh endpoint key temp file: %w", err)
	}
	if _, err := tmpFile.Write(pemBytes); err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("write iroh endpoint key: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close iroh endpoint key temp file: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("install iroh endpoint key: %w", err)
		}
		// Lost the race: another process wrote a complete key first, and its
		// key is the server's identity now.
		existing, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read iroh endpoint key: %w", err)
		}
		return parseEndpointKey(existing)
	}
	return priv, nil
}

// EndpointID is the address the given secret key answers on.
func EndpointID(key ed25519.PrivateKey) (endpoint.IrohID, error) {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return endpoint.IrohID{}, errors.New("iroh endpoint key is not ed25519")
	}
	return endpoint.IrohIDFromPublicKey(pub)
}

func marshalEndpointKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal iroh endpoint key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseEndpointKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("iroh endpoint key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse iroh endpoint key: %w", err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("iroh endpoint key is %T, want ed25519", parsed)
	}
	return key, nil
}
