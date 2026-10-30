// Package agenttls is the keypair the sandbox agent serves its API over TLS
// with, where its pool serves its sandboxes' agents over TLS (ADR
// 26-10-29-481 §3): one the pool's mTLS CA signed for the sandbox's private
// name, placed in the config volume before the agent starts and renewed in the
// runtime-config document.
package agenttls

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// Keypair is the agent's server keypair as the files in its directory hold
// it, read afresh for every handshake and loaded again whenever what they hold
// changes, so a renewed keypair is served from the next handshake and nothing
// is restarted for it. It goes by what the files hold, not by their size and
// time: a renewal is almost always the same size, and a machine's place step
// writes its files with the same time every start.
type Keypair struct {
	cert, key string

	mu      sync.Mutex
	certPEM []byte
	keyPEM  []byte
	loaded  *tls.Certificate
}

// NewKeypair reads the keypair in dir (sandboxconfig.AgentTLSCertName and
// AgentTLSKeyName), which need not exist yet.
func NewKeypair(dir string) *Keypair {
	return &Keypair{
		cert: filepath.Join(dir, sandboxconfig.AgentTLSCertName),
		key:  filepath.Join(dir, sandboxconfig.AgentTLSKeyName),
	}
}

// Certificate is the keypair as the files hold it now.
func (k *Keypair) Certificate() (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(k.cert)
	if err != nil {
		return nil, fmt.Errorf("the agent's TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(k.key)
	if err != nil {
		return nil, fmt.Errorf("the agent's TLS key: %w", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.loaded != nil && bytes.Equal(certPEM, k.certPEM) && bytes.Equal(keyPEM, k.keyPEM) {
		return k.loaded, nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("load the agent's TLS keypair: %w", err)
	}
	k.loaded, k.certPEM, k.keyPEM = &pair, certPEM, keyPEM
	return k.loaded, nil
}

// Config is the server TLS configuration that serves keypair. The pool
// authenticates itself with a token on every request, so no client
// certificate is asked for.
func Config(keypair *Keypair) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return keypair.Certificate()
		},
	}
}
