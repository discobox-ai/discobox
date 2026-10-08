package proxyagent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/discobox-ai/discobox/pool-agent/dnsforward"
	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

const (
	// DNSListenAddress is where the pool answers its sandboxes' DNS, over TLS
	// with the same certificates as the proxy (dnsforward). It binds all
	// interfaces for the reason the proxy does; the client certificate, not
	// the network, is what admits a caller. TestPoolListenAddressesAreDistinct
	// guards the port.
	DNSListenAddress = "0.0.0.0:17085"

	// DNSServerAddress is where a sandbox's DNS stub dials. It verifies the
	// pool's certificate for ServerName, so an answer can only come from the
	// pool, whoever else is on the network.
	DNSServerAddress = ServerName + ":17085"
)

// SandboxDNSAddress is the DNS server every sandbox container is created with.
// It is link-local and belongs to nobody on the network: the sandbox's DNS stub
// claims it on the sandbox's own loopback, so Docker's embedded resolver hands
// the stub what it cannot answer, without that traffic leaving the sandbox. The
// address is the sandbox's (sandboxconfig.SandboxDNSAddress); the pool reads it
// only to hand it to the container runtime (ADR 26-10-08-127 §4).
var SandboxDNSAddress = netip.MustParseAddr(sandboxconfig.SandboxDNSAddress)

// serveDNS answers sandboxes' DNS until ctx is done. The upstream is this
// container's own resolver, which reaches the outside; every query is audited
// into server's trail, beside the sandbox's HTTP.
func serveDNS(ctx context.Context, logger *slog.Logger, bundle *proxy.CertificateBundle, server *proxy.Server) error {
	upstream, err := dnsforward.SystemUpstream("/etc/resolv.conf")
	if err != nil {
		return fmt.Errorf("find the pool's resolver: %w", err)
	}
	var listenConfig net.ListenConfig
	tcp, err := listenConfig.Listen(ctx, "tcp", DNSListenAddress)
	if err != nil {
		return err
	}
	logger.Info("pool sandbox dns serving", "addr", tcp.Addr(), "upstream", upstream)
	dnsforward.New(logger, upstream, server.RecordDNS).Serve(ctx, tls.NewListener(tcp, sandboxTLSConfig(bundle)))
	return nil
}

// sandboxTLSConfig is the server side of every mTLS endpoint the pool offers
// its sandboxes: the proxy's server certificate, and a required client
// certificate from the pool's CA whose common name is the sandbox.
func sandboxTLSConfig(bundle *proxy.CertificateBundle) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{bundle.ServerCert},
		ClientCAs:    bundle.ClientCAPool,
		// The client certificate is the identity, so an unverified client is not
		// an anonymous caller to be authenticated some other way — it is no
		// caller at all.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS12,
	}
}
