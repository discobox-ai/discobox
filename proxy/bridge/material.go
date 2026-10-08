package bridge

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Material is the sandbox's mTLS material — the pool's mTLS CA and the
// sandbox's client keypair — read from the files the intake writes, and read
// again whenever they change. The pool renews a running sandbox's certificate
// by delivering a new runtime-config document (ADR 0126 §7, ADR 26-10-08-127),
// which replaces these files under whatever already holds them, so every
// handshake starts from the files as they are now: a renewed certificate or a
// new CA is presented from the next connection, with nothing restarted.
//
// The intake replaces the files one at a time, so a handshake can find a new
// certificate beside the old key. A set that does not load keeps the last one
// that did, and is tried again on the next handshake. A certificate outside
// its validity is never presented: a handshake that would have to is refused
// here, and waits for the renewal, rather than being refused by the pool.
type Material struct {
	caPath   string
	certPath string
	keyPath  string
	// now is the clock validity is judged by.
	now func() time.Time

	mu   sync.Mutex
	held materialFiles
	// failed is the set of files that last failed to load, so a set that stays
	// broken is reported once rather than on every handshake.
	failed materialFiles
	roots  *x509.CertPool
	pair   tls.Certificate
	leaf   *x509.Certificate
}

// materialFiles is the contents of the three files a Material reads.
type materialFiles struct {
	ca, cert, key []byte
}

func (f materialFiles) equal(o materialFiles) bool {
	return bytes.Equal(f.ca, o.ca) && bytes.Equal(f.cert, o.cert) && bytes.Equal(f.key, o.key)
}

// LoadMaterial reads the material from its files. It fails when they cannot be
// read or do not form a usable set; a certificate already out of date is
// reported by TLSConfig, so a bridge started on an expired certificate is up
// and serves again once the renewal lands.
func LoadMaterial(caPath, certPath, keyPath string) (*Material, error) {
	m := &Material{caPath: caPath, certPath: certPath, keyPath: keyPath, now: time.Now}
	files, err := m.read()
	if err != nil {
		return nil, err
	}
	if err := m.load(files); err != nil {
		return nil, err
	}
	return m, nil
}

// TLSConfig is a client configuration for one handshake with the pool service
// whose certificate is issued for serverName: the CA and keypair as the files
// hold them now. It fails when the certificate it would present is expired or
// not yet valid.
func (m *Material) TLSConfig(serverName string) (*tls.Config, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	now := m.now()
	switch {
	case now.After(m.leaf.NotAfter):
		return nil, fmt.Errorf("client certificate %s expired at %s; waiting for the pool to renew it", m.certPath, m.leaf.NotAfter.UTC().Format(time.RFC3339))
	case now.Before(m.leaf.NotBefore):
		return nil, fmt.Errorf("client certificate %s is not valid until %s", m.certPath, m.leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	return &tls.Config{
		RootCAs:      m.roots,
		Certificates: []tls.Certificate{m.pair},
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// refresh loads the files again when they differ from the set held. The caller
// holds mu.
func (m *Material) refresh() {
	files, err := m.read()
	if err == nil && files.equal(m.held) {
		return
	}
	if err == nil {
		err = m.load(files)
	}
	if err == nil {
		m.failed = materialFiles{}
		return
	}
	if !files.equal(m.failed) {
		slog.Warn("reload sandbox mTLS material; keeping the previous", "cert", m.certPath, "error", err)
		m.failed = files
	}
}

func (m *Material) read() (materialFiles, error) {
	var files materialFiles
	var errs []error
	for _, piece := range []struct {
		into *[]byte
		path string
		what string
	}{
		{&files.ca, m.caPath, "mTLS CA"},
		{&files.cert, m.certPath, "client certificate"},
		{&files.key, m.keyPath, "client key"},
	} {
		data, err := os.ReadFile(piece.path)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", piece.what, err))
		}
		*piece.into = data
	}
	return files, errors.Join(errs...)
}

// load parses files and, when they form a usable set, holds them. The caller
// holds mu, or has not shared m yet.
func (m *Material) load(files materialFiles) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(files.ca) {
		return errors.New("parse mTLS CA")
	}
	pair, err := tls.X509KeyPair(files.cert, files.key)
	if err != nil {
		return fmt.Errorf("load client certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse client certificate: %w", err)
	}
	m.held, m.roots, m.pair, m.leaf = files, roots, pair, leaf
	return nil
}
