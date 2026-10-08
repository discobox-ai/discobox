package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"github.com/discobox-ai/discobox/wire"
)

// DialConfig names a pool service and the sandbox material that authenticates
// to it.
type DialConfig struct {
	// URL is where the service is. Its scheme picks the transport through
	// wire: https dials TCP, vsock dials an AF_VSOCK context and port, and unix
	// dials a socket. Every one of them carries mTLS; http is refused, because
	// nothing here speaks plaintext to the pool.
	URL string
	// ServerName is the name the pool's server certificate is verified as.
	// Empty takes the URL's host name, which an https URL has and a vsock or
	// unix URL does not.
	ServerName string

	MTLSCAPath     string
	ClientCertPath string
	ClientKeyPath  string
}

// Dialer reaches a pool service over mTLS, through whatever transport its URL
// names. The client certificate's common name is the sandbox's identity at the
// pool, so the service on the other end is the same whatever carried the bytes
// (ADR 0144 §4).
type Dialer struct {
	url       string
	serverURL string
	address   string
	dial      func(context.Context, string, string) (net.Conn, error)
	tlsConfig *tls.Config
}

// NewDialer loads the sandbox's mTLS material and resolves the transport cfg.URL
// names.
func NewDialer(cfg DialConfig) (*Dialer, error) {
	endpoint, err := wire.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("pool URL: %w", err)
	}
	serverName := cfg.ServerName
	var address, serverURL string
	switch endpoint.Scheme {
	case "https":
		// SplitHostPort accepts ":17080" and "pool:", so an empty half is
		// refused here: either one fails every connection rather than this
		// one call, and a failure at startup is the one a log shows once.
		host, port, err := net.SplitHostPort(endpoint.Host)
		if err != nil {
			return nil, fmt.Errorf("pool URL %q must name a host and port: %w", cfg.URL, err)
		}
		if host == "" || port == "" {
			return nil, fmt.Errorf("pool URL %q must name a host and port", cfg.URL)
		}
		if serverName == "" {
			serverName = host
		}
		address = endpoint.Host
		serverURL = endpoint.BaseURL()
	case "vsock", "unix":
		if serverName == "" {
			return nil, fmt.Errorf("pool URL %q names no host to verify the pool's certificate as; a server name is required", cfg.URL)
		}
		// The dialer fixes the peer, so the authority only has to be the name
		// the certificate is verified as.
		serverURL = "https://" + serverName
	default:
		return nil, fmt.Errorf("pool URL %q must be https, vsock, or unix: the pool is reached over mTLS", cfg.URL)
	}
	dial, err := endpoint.DialContext()
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(cfg.MTLSCAPath)
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse mTLS CA")
	}
	clientCert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	return &Dialer{
		url:       cfg.URL,
		serverURL: serverURL,
		address:   address,
		dial:      dial,
		tlsConfig: &tls.Config{
			RootCAs:      caPool,
			Certificates: []tls.Certificate{clientCert},
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS12,
		},
	}, nil
}

// Dial opens the transport and completes the mTLS handshake over it.
func (d *Dialer) Dial(ctx context.Context) (*tls.Conn, error) {
	raw, err := d.dial(ctx, "tcp", d.address)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, d.tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

// DialTLSContext is Dial in the shape http.Transport installs: the transport
// already fixes the peer, so the network and address it is asked for are
// ignored. A client using it sends its requests to ServerURL.
func (d *Dialer) DialTLSContext(ctx context.Context, _, _ string) (net.Conn, error) {
	return d.Dial(ctx)
}

// ServerURL is the https base URL to address requests to: the configured URL
// for https, and the server name for a transport that names no host.
func (d *Dialer) ServerURL() string {
	return d.serverURL
}

// URL is the pool URL as configured, for logs and traces.
func (d *Dialer) URL() string {
	return d.url
}
