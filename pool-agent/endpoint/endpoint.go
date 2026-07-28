// Package endpoint resolves the pool agent's transport from a URL, so that the
// scheme — and nothing else — decides how bytes reach the other side.
//
// The control plane and the pool agent address each other with a single URL
// each. Adding a backend means teaching this package a scheme, not threading a
// new field through the engine, the boot environment, the bootstrap contract,
// and the agent:
//
//	local docker   http://host.docker.internal:8080   plain IP transport
//	libkrun        vsock://2:3001                     AF_VSOCK to host CID 2
//	wslc           unix:///run/discobox/cp.sock       a socket the guest helper serves
//
// The same vocabulary describes where the agent listens (Listen), so a backend
// that terminates the agent API on VSOCK or a Unix socket needs no special case
// above the transport layer either.
//
// This mirrors localipc, which already resolves the CLI-to-server transport by
// scheme; the two are separate because they serve different hops and different
// scheme sets.
package endpoint

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/obot-platform/discobox/pool-agent/vsock"
)

// LogicalHTTPBaseURL is the base URL used when the transport is not addressed
// by host name. The dialer already determines the peer, so the authority only
// has to be stable and valid.
const LogicalHTTPBaseURL = "http://pool-agent.local"

// Endpoint is a parsed transport URL.
type Endpoint struct {
	// Raw is the URL as configured.
	Raw string
	// Scheme selects the transport: http, https, vsock, or unix.
	Scheme string
	// Host is the IP host for http/https and the context ID for vsock. It is
	// empty for unix and for a vsock listen address.
	Host string
	// Port is the TCP port for http/https and the port number for vsock.
	Port uint32
	// Path is the socket path for unix.
	Path string
}

// Parse validates a transport URL.
func Parse(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("endpoint is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse endpoint %q: %w", raw, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
		if parsed.Host == "" {
			return Endpoint{}, fmt.Errorf("%s endpoint %q must include a host", scheme, raw)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Host: parsed.Host}, nil
	case "vsock":
		host, port, err := splitVSOCK(parsed.Host, raw)
		if err != nil {
			return Endpoint{}, err
		}
		return Endpoint{Raw: raw, Scheme: scheme, Host: host, Port: port}, nil
	case "unix":
		path := parsed.Path
		if path == "" {
			path = parsed.Opaque
		}
		if strings.TrimSpace(path) == "" {
			return Endpoint{}, fmt.Errorf("unix endpoint %q must include a socket path", raw)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Path: path}, nil
	default:
		return Endpoint{}, fmt.Errorf("unsupported endpoint scheme %q in %q", parsed.Scheme, raw)
	}
}

// splitVSOCK parses a vsock authority. The context ID is optional so that
// "vsock://:3002" describes a listen address, while "vsock://2:3001" dials host
// CID 2.
func splitVSOCK(authority, raw string) (string, uint32, error) {
	if authority == "" {
		return "", 0, fmt.Errorf("vsock endpoint %q must include a port", raw)
	}
	host, portText, err := net.SplitHostPort(authority)
	if err != nil {
		return "", 0, fmt.Errorf("vsock endpoint %q must be cid:port or :port: %w", raw, err)
	}
	port, err := strconv.ParseUint(portText, 10, 32)
	if err != nil {
		return "", 0, fmt.Errorf("vsock endpoint %q has an invalid port: %w", raw, err)
	}
	if port < 1024 {
		return "", 0, fmt.Errorf("vsock endpoint %q port must be at least 1024", raw)
	}
	if host != "" {
		if _, err := strconv.ParseUint(host, 10, 32); err != nil {
			return "", 0, fmt.Errorf("vsock endpoint %q context ID must be numeric: %w", raw, err)
		}
	}
	return host, uint32(port), nil
}

// contextID resolves a vsock endpoint's context ID, defaulting to the host.
func (e Endpoint) contextID() (uint32, error) {
	if e.Host == "" {
		return vsock.HostCID, nil
	}
	cid, err := strconv.ParseUint(e.Host, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("vsock endpoint %q context ID must be numeric: %w", e.Raw, err)
	}
	return uint32(cid), nil
}

// DialContext returns the dialer for this endpoint, suitable for installing as
// http.Transport.DialContext.
func (e Endpoint) DialContext() (func(context.Context, string, string) (net.Conn, error), error) {
	switch e.Scheme {
	case "http", "https":
		dialer := &net.Dialer{}
		return dialer.DialContext, nil
	case "vsock":
		cid, err := e.contextID()
		if err != nil {
			return nil, err
		}
		return vsock.DialContextCID(cid, e.Port), nil
	case "unix":
		dialer := &net.Dialer{}
		path := e.Path
		return func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", path)
		}, nil
	default:
		return nil, fmt.Errorf("unsupported endpoint scheme %q", e.Scheme)
	}
}

// IsIP reports whether the endpoint is reached over IP, and therefore has a TCP
// port that a container runtime can publish and a TCP health check can probe.
func (e Endpoint) IsIP() bool {
	return e.Scheme == "http" || e.Scheme == "https"
}

// BaseURL is the HTTP base URL to send requests to. For IP transports it is the
// configured URL; otherwise the dialer already fixes the peer, so a stable
// logical authority is used.
func (e Endpoint) BaseURL() string {
	if e.Scheme == "http" || e.Scheme == "https" {
		return strings.TrimRight(e.Raw, "/")
	}
	return LogicalHTTPBaseURL
}

// HTTPClient resolves a URL into the base URL to use and a client that reaches
// it. This is the single place a caller needs in order to talk to a peer
// without knowing the transport.
func HTTPClient(raw string, timeout time.Duration) (string, *http.Client, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return "", nil, err
	}
	dial, err := parsed.DialContext()
	if err != nil {
		return "", nil, err
	}
	return parsed.BaseURL(), &http.Client{
		Transport: &http.Transport{DialContext: dial},
		Timeout:   timeout,
	}, nil
}

// Listen binds the listener described by a URL, for the side that serves.
// "vsock://:3002" listens on a VSOCK port, "unix:///path" on a socket, and
// "http://0.0.0.0:3002" on TCP.
func Listen(raw string) (net.Listener, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	switch parsed.Scheme {
	case "http", "https":
		var config net.ListenConfig
		return config.Listen(context.Background(), "tcp", parsed.Host)
	case "vsock":
		return vsock.Listen(parsed.Port)
	case "unix":
		var config net.ListenConfig
		return config.Listen(context.Background(), "unix", parsed.Path)
	default:
		return nil, fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
}

// VSOCKURL renders a dial URL for a VSOCK context ID and port.
func VSOCKURL(cid, port uint32) string {
	return fmt.Sprintf("vsock://%d:%d", cid, port)
}

// VSOCKListenURL renders a listen URL for a VSOCK port.
func VSOCKListenURL(port uint32) string {
	return fmt.Sprintf("vsock://:%d", port)
}

// TCPListenURL renders a listen URL for a TCP port on all interfaces.
func TCPListenURL(port int) string {
	return "http://0.0.0.0:" + strconv.Itoa(port)
}
