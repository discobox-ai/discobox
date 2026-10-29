package buildkitagent

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/proxy"
)

// The registry speaks mTLS on the pool's loopback, and admits only a client
// holding the pool builder's certificate: buildkitd, which presents it for
// RegistryHost alone, and the registry front (ADR 26-10-29-160 §3).
//
// That is what makes a name binding at the mediator sound. buildkitd resolves
// whatever name a build gives it, and anything that resolves to loopback
// reaches this port; without a client certificate it is refused, so the one
// name a build can reach the registry by is RegistryHost itself, which the
// mediator matches exactly (bindToNamespace).

// registryClientID is the common name of the one client certificate the
// registry admits.
const registryClientID = "pool-builder"

// registryTLS is where the registry's CA, its server keypair and the pool
// builder's client keypair are, as files the units read.
type registryTLS struct {
	CA         string
	ServerCert string
	ServerKey  string
	ClientCert string
	ClientKey  string
}

// RegistryTLSDir holds the registry's certificate material, on the pool's
// disposable build tree beside the registry, which no sandbox can reach (ADR
// 0050). A CA of its own, not the proxy's: the proxy's mTLS CA signs every
// sandbox's client certificate, and the registry must admit none of them.
func RegistryTLSDir(root layout.Root, projectID, poolID string) string {
	return filepath.Join(root.PoolBuild(projectID, poolID), "registry-tls")
}

// ensureRegistryTLS creates the registry's certificate material, or reuses
// what an earlier start made: a registry and a builder that are still running
// keep trusting what they loaded.
func ensureRegistryTLS(root layout.Root, projectID, poolID string) (*registryTLS, error) {
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         RegistryTLSDir(root, projectID, poolID),
		ServerHosts: []string{registryIP},
		ClientIDs:   []string{registryClientID},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare registry certificates: %w", err)
	}
	client := prepared.Clients[registryClientID]
	return &registryTLS{
		CA:         prepared.Bundle.MTLSCAPath,
		ServerCert: prepared.Bundle.ServerCertPath,
		ServerKey:  prepared.Bundle.ServerKeyPath,
		ClientCert: client.ClientCertPath,
		ClientKey:  client.ClientKeyPath,
	}, nil
}

// RegistryClientTLS is the TLS configuration the registry front reaches the
// registry with: the pool builder's certificate, and the registry's CA alone.
func RegistryClientTLS(root layout.Root, projectID, poolID string) (*tls.Config, error) {
	material, err := ensureRegistryTLS(root, projectID, poolID)
	if err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(material.ClientCert, material.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load the registry client certificate: %w", err)
	}
	caPEM, err := readFile(material.CA)
	if err != nil {
		return nil, fmt.Errorf("read the registry CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse the registry CA")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
