package bridge

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"github.com/discobox-ai/discobox/pkg/wire"
	"github.com/discobox-ai/discobox/pkg/wstunnel"
)

// DialConfig names a pool service and the sandbox material that authenticates
// to it.
type DialConfig struct {
	// URL is where the service is. Its scheme picks the transport through
	// wire: https dials TCP, vsock dials an AF_VSOCK context and port, unix
	// dials a socket, and wss opens a WebSocket tunnel through an HTTPS edge
	// (ADR 26-10-23-719). Every one of them carries mTLS; http is refused,
	// because nothing here speaks plaintext to the pool.
	URL string
	// ServerName is the name the pool's server certificate is verified as.
	// Empty takes the URL's host name, which an https URL has and a vsock or
	// unix URL does not. A wss URL's host is the edge's, never the pool's, so
	// it needs one too.
	ServerName string

	MTLSCAPath     string
	ClientCertPath string
	ClientKeyPath  string
}

// Dialer reaches a pool service over mTLS, through whatever transport its URL
// names. The client certificate's common name is the sandbox's identity at the
// pool, so the service on the other end is the same whatever carried the bytes
// (ADR 0144 §4). Each handshake reads the material as it is then (Material), so
// a certificate the pool renews is presented without the Dialer being rebuilt.
type Dialer struct {
	url        string
	serverURL  string
	serverName string
	// transportHost is the name the transport itself resolves to reach the
	// peer: an https URL's host, or a wss URL's edge. vsock and unix have none.
	transportHost string
	address       string
	// dial opens the transport, except for wss, which tunnel opens.
	dial     func(context.Context, string, string) (net.Conn, error)
	tunnel   *wstunnel.Client
	material *Material
}

// NewDialer loads the sandbox's mTLS material and resolves the transport cfg.URL
// names.
func NewDialer(cfg DialConfig) (*Dialer, error) {
	endpoint, err := wire.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("pool URL: %w", err)
	}
	serverName := cfg.ServerName
	var address, serverURL, transportHost string
	var dial func(context.Context, string, string) (net.Conn, error)
	var tunnel *wstunnel.Client
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
		transportHost = host
	case "wss":
		if serverName == "" {
			return nil, fmt.Errorf("pool URL %q names the edge, not the pool; a server name to verify the pool's certificate as is required", cfg.URL)
		}
		serverURL = "https://" + serverName
		transportHost = endpoint.Host
		if host, _, err := net.SplitHostPort(endpoint.Host); err == nil {
			transportHost = host
		}
		tunnel = wstunnel.NewClient(endpoint.Raw, nil)
	case "vsock", "unix":
		if serverName == "" {
			return nil, fmt.Errorf("pool URL %q names no host to verify the pool's certificate as; a server name is required", cfg.URL)
		}
		// The dialer fixes the peer, so the authority only has to be the name
		// the certificate is verified as.
		serverURL = "https://" + serverName
	default:
		return nil, fmt.Errorf("pool URL %q must be https, vsock, unix, or wss: the pool is reached over mTLS", cfg.URL)
	}
	if tunnel == nil {
		if dial, err = endpoint.DialContext(); err != nil {
			return nil, err
		}
	}
	material, err := LoadMaterial(cfg.MTLSCAPath, cfg.ClientCertPath, cfg.ClientKeyPath)
	if err != nil {
		return nil, err
	}
	return &Dialer{
		url:           cfg.URL,
		serverURL:     serverURL,
		serverName:    serverName,
		transportHost: strings.ToLower(strings.TrimSuffix(transportHost, ".")),
		address:       address,
		dial:          dial,
		tunnel:        tunnel,
		material:      material,
	}, nil
}

// Dial opens the transport and completes the mTLS handshake over it. It fails
// without dialing when the client certificate is out of date.
func (d *Dialer) Dial(ctx context.Context) (*tls.Conn, error) {
	tlsConfig, err := d.material.TLSConfig(d.serverName)
	if err != nil {
		return nil, err
	}
	if d.tunnel != nil {
		return d.dialTunnel(ctx, tlsConfig)
	}
	raw, err := d.dial(ctx, "tcp", d.address)
	if err != nil {
		return nil, err
	}
	return handshake(ctx, raw, tlsConfig)
}

// dialTunnel runs the handshake on a stream through the edge. A tunnel reused
// from an earlier stream may have been cut by the edge while it sat idle, which
// shows first in the handshake, before any of the caller's bytes are sent; that
// one failure is retried on a new tunnel (ADR 26-10-23-719 §3).
func (d *Dialer) dialTunnel(ctx context.Context, tlsConfig *tls.Config) (*tls.Conn, error) {
	stream, err := d.tunnel.Dial(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := handshake(ctx, stream, tlsConfig)
	if err == nil || !stream.Reused() || ctx.Err() != nil {
		return conn, err
	}
	if stream, err = d.tunnel.DialNew(ctx); err != nil {
		return nil, err
	}
	return handshake(ctx, stream, tlsConfig)
}

func handshake(ctx context.Context, raw net.Conn, tlsConfig *tls.Config) (*tls.Conn, error) {
	conn := tls.Client(raw, tlsConfig)
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

// ServerName is the name the pool's certificate is verified as.
func (d *Dialer) ServerName() string {
	return d.serverName
}

// TransportHost is the name, lower-cased, that the transport resolves to reach
// the pool: an https URL's host, or a wss URL's edge. It is empty for vsock and
// unix, which resolve nothing.
func (d *Dialer) TransportHost() string {
	return d.transportHost
}

// URL is the pool URL as configured, for logs and traces.
func (d *Dialer) URL() string {
	return d.url
}
