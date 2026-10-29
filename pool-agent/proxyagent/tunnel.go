package proxyagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/wstunnel"
)

const (
	// EnvTunnelURL is the wss base URL of the HTTPS edge this pool sits behind,
	// for a pool its sandboxes reach only through one (ADR 26-10-23-719). It is
	// set on the pool container by whatever puts the pool behind the edge; no
	// provider sets it yet. Unset, the pool serves no tunnels and its sandboxes
	// reach it directly.
	EnvTunnelURL = "DISCOBOX_POOL_TUNNEL_URL"

	// TunnelListenAddress is where the pool serves tunnels, in plain HTTP: the
	// edge holds the public certificate and presents none of its own, and the
	// mTLS inside each tunnel is what authenticates and protects it. The edge
	// forwards to this port. TestPoolListenAddressesAreDistinct guards it.
	TunnelListenAddress = "0.0.0.0:17090"

	// maxTunnels bounds the tunnels the listener holds, idle or carrying a
	// stream, so a flood through the edge costs the proxy unit a bounded number
	// of descriptors: an idle tunnel holds one, one carrying a stream three,
	// so at most about 25,000. The proxy unit sets no LimitNOFILE of its own,
	// and Go raises its soft limit to the hard one it inherits from the pool
	// container, commonly 1048576. It cannot be per caller: every
	// tunnel arrives from the edge, and nothing names a sandbox until the mTLS
	// inside a stream completes at the service, so limiting each client is the
	// edge's (ADR 26-10-23-719 §4). It is sized for real use: a sandbox holds a
	// tunnel per open connection and up to 16 idle per dialer for 30s, and a
	// busy one opens hundreds. A tunnel carries one stream at a time, so this
	// bounds streams too.
	maxTunnels = 8192
)

// TunnelSource is the loopback address the tunnel listener splices streams
// from. A service that bounds callers by source address before a certificate
// names them (dnsforward) exempts it, since every sandbox behind the edge
// arrives from it, and caps every other loopback source as one: the pool proxy
// carries a sandbox's own connection to a loopback port from 127.0.0.1, and a
// sandbox cannot choose the address it is carried from. The tunnel listener
// bounds what it splices itself (maxTunnels).
var TunnelSource = netip.MustParseAddr("127.0.0.2")

// The path of each service on the tunnel listener. The builder's is the
// mediator's: a build's results come back through the registry host, which the
// proxy's tunnel already carries, so a pool behind an edge builds like any
// other (ADR 26-10-29-160 §5).
const (
	tunnelProxyPath       = "/tunnel/proxy"
	tunnelCredentialsPath = "/tunnel/credentials" //nolint:gosec // G101: a URL path, not a credential.
	tunnelDNSPath         = "/tunnel/dns"
	tunnelBuildKitPath    = "/tunnel/buildkit"
)

// tunnelServices names each service a tunnel reaches by its path, and the
// service's own mTLS listener in this pool container, which the tunnel's stream
// is spliced onto. The mTLS inside the stream ends at that listener exactly as
// it would over TCP, so no service learns a tunnel carried it.
var tunnelServices = map[string]string{
	tunnelProxyPath:       loopback(ListenAddress),
	tunnelCredentialsPath: loopback(CredentialsListenAddress),
	tunnelDNSPath:         loopback(DNSListenAddress),
	tunnelBuildKitPath:    net.JoinHostPort("127.0.0.1", mediatorPort),
}

// loopback is the port of a listen address on this container's loopback.
func loopback(hostPort string) string {
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		panic(fmt.Sprintf("pool service address %q: %v", hostPort, err))
	}
	return net.JoinHostPort("127.0.0.1", port)
}

// ConfiguredTunnelURL resolves EnvTunnelURL, returning "" when it is unset.
func ConfiguredTunnelURL() (string, error) {
	value := strings.TrimSpace(os.Getenv(EnvTunnelURL))
	if value == "" {
		return "", nil
	}
	if err := ValidateTunnelURL(value); err != nil {
		return "", fmt.Errorf("%s: %w", EnvTunnelURL, err)
	}
	return strings.TrimRight(value, "/"), nil
}

// ValidateTunnelURL checks a pool tunnel URL: wss, a host, and nothing after
// it. The pool appends each service's path itself.
func ValidateTunnelURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "wss" {
		return fmt.Errorf("%q must be a wss URL", value)
	}
	if parsed.Hostname() == "" {
		return fmt.Errorf("%q names no host", value)
	}
	if strings.Trim(parsed.Path, "/") != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("%q must name only a host and port: the pool adds each service's path", value)
	}
	return nil
}

// serveTunnels accepts the edge's tunnels until ctx ends, splicing each stream
// onto the listener of the service its path names.
func serveTunnels(ctx context.Context, logger *slog.Logger) error {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", TunnelListenAddress)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           newTunnelHandler(ctx, logger),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	logger.Info("pool tunnels serving", "addr", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newTunnelHandler serves every service's tunnel path, within maxTunnels.
func newTunnelHandler(ctx context.Context, logger *slog.Logger) http.Handler {
	tunnels := make(chan struct{}, maxTunnels)
	mux := http.NewServeMux()
	for path, target := range tunnelServices {
		mux.Handle(path, wstunnel.Handler(ctx, func(stream net.Conn) {
			spliceTunnelStream(ctx, logger, stream, target)
		}))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case tunnels <- struct{}{}:
		default:
			http.Error(w, "too many tunnels", http.StatusServiceUnavailable)
			return
		}
		defer func() { <-tunnels }()
		mux.ServeHTTP(w, r)
	})
}

// spliceTunnelStream carries one stream to target and back, from TunnelSource,
// closing each side when its direction ends, as the sandbox's bridge does on
// its end.
func spliceTunnelStream(ctx context.Context, logger *slog.Logger, stream net.Conn, target string) {
	defer func() { _ = stream.Close() }()
	dialer := net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(TunnelSource, 0))}
	service, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		logger.Warn("pool tunnel could not reach its service", "target", target, "error", err)
		return
	}
	defer func() { _ = service.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(service, stream)
		_ = service.Close()
	}()
	_, _ = io.Copy(stream, service)
	_ = stream.Close()
	<-done
}
