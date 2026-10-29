package buildkitagent

import (
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"

	"google.golang.org/grpc"
)

// NewTestMediator is a mediator with no upstream connection, for exercising the
// parts of it that do not forward.
func NewTestMediator(logger *slog.Logger) *Mediator { return &Mediator{logger: logger} }

// Drain stops srv the way a shutdown does, bounded by within.
func (m *Mediator) Drain(srv *grpc.Server, within time.Duration) { m.drain(srv, within) }

// RegistryServerTLS is the TLS configuration the registry serves with, as its
// environment file names it: its server keypair, requiring a client
// certificate from its CA.
func RegistryServerTLS(root layout.Root, projectID, poolID string) (*tls.Config, error) {
	material, err := ensureRegistryTLS(root, projectID, poolID)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(material.ServerCert, material.ServerKey)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(material.CA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return &tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}, nil
}
