package bridge_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy"
	"github.com/discobox-ai/discobox/proxy/bridge"
)

const poolServerName = "discobox-pool-proxy"

// poolMaterial issues the pool's server certificate and one sandbox's client
// certificate the way the pool does.
func poolMaterial(t *testing.T) *proxy.PreparedCertificates {
	t.Helper()
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{
		Dir:         filepath.Join(t.TempDir(), "certs"),
		ServerHosts: []string{poolServerName, "127.0.0.1"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates: %v", err)
	}
	return prepared
}

// servePool stands in for a pool service on listener: it requires a client
// certificate signed by the pool's mTLS CA, answers each connection with the
// certificate's common name, and then echoes.
func servePool(t *testing.T, listener net.Listener, bundle *proxy.CertificateBundle) {
	t.Helper()
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{bundle.ServerCert},
		ClientCAs:    bundle.ClientCAPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})
	t.Cleanup(func() { _ = tlsListener.Close() })
	go func() {
		for {
			conn, err := tlsListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				tlsConn := conn.(*tls.Conn)
				if err := tlsConn.HandshakeContext(context.Background()); err != nil {
					return
				}
				peer := tlsConn.ConnectionState().PeerCertificates[0]
				if _, err := io.WriteString(conn, peer.Subject.CommonName+"\n"); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
}

// unixPool serves a pool service on a Unix socket and returns its URL. The
// directory is short on purpose: a socket path has a length limit that
// t.TempDir can exceed.
func unixPool(t *testing.T, bundle *proxy.CertificateBundle) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "br")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "pool.sock")
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	servePool(t, listener, bundle)
	return "unix://" + socket
}

func dialConfig(material proxy.ClientMaterial, url, serverName string) bridge.DialConfig {
	return bridge.DialConfig{
		URL:            url,
		ServerName:     serverName,
		MTLSCAPath:     material.MTLSCAPath,
		ClientCertPath: material.ClientCertPath,
		ClientKeyPath:  material.ClientKeyPath,
	}
}

// readIdentity reads the common name the pool saw on conn.
func readIdentity(t *testing.T, conn net.Conn) (*bufio.Reader, string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	return reader, strings.TrimSuffix(line, "\n")
}

// A sandbox in a host-VM pool reaches its pool through a socket rather than a
// network (ADR 0144 §4). The forwarder must dial the unix URL through wire and
// still present the sandbox's client certificate, so the pool sees the same
// tenant identity it would over TCP.
func TestForwarderDialsUnixURLWithMTLS(t *testing.T) {
	prepared := poolMaterial(t)
	url := unixPool(t, prepared.Bundle)
	material := prepared.Clients["sandbox-1"]

	forwarder, err := bridge.New(context.Background(), bridge.Config{
		WorkerProxyURL: url,
		ServerName:     poolServerName,
		MTLSCAPath:     material.MTLSCAPath,
		ClientCertPath: material.ClientCertPath,
		ClientKeyPath:  material.ClientKeyPath,
	})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	var listenConfig net.ListenConfig
	local, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = forwarder.Serve(local) }()
	t.Cleanup(func() { _ = forwarder.Close() })

	var dialer net.Dialer
	conn, err := dialer.DialContext(context.Background(), "tcp", local.Addr().String())
	if err != nil {
		t.Fatalf("dial forwarder: %v", err)
	}
	defer func() { _ = conn.Close() }()
	reader, identity := readIdentity(t, conn)
	if identity != "sandbox-1" {
		t.Fatalf("pool saw client %q, want sandbox-1", identity)
	}
	// Plaintext in, plaintext back: the forwarder splices the sandbox's bytes
	// onto the mTLS connection unchanged.
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if echo != "ping\n" {
		t.Fatalf("echo = %q, want %q", echo, "ping\n")
	}
}

// Changing the transport must not weaken the pool's authentication: over a
// socket the pool's certificate is still verified against the server name.
func TestDialerVerifiesPoolCertificateOverUnix(t *testing.T) {
	prepared := poolMaterial(t)
	url := unixPool(t, prepared.Bundle)
	dialer, err := bridge.NewDialer(dialConfig(prepared.Clients["sandbox-1"], url, "not-the-pool"))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialer.Dial(ctx)
	if err == nil {
		_ = conn.Close()
		t.Fatal("Dial succeeded against a certificate for another name")
	}
}

// A Docker pool's bridge.json names https://discobox-pool-proxy:17080. An https
// URL still dials TCP and takes the server name from its host, as before.
func TestDialerHTTPSDialsTCPAndTakesServerNameFromHost(t *testing.T) {
	prepared := poolMaterial(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	servePool(t, listener, prepared.Bundle)
	url := "https://" + listener.Addr().String()

	dialer, err := bridge.NewDialer(dialConfig(prepared.Clients["sandbox-1"], url, ""))
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	if got := dialer.ServerURL(); got != url {
		t.Fatalf("ServerURL = %q, want %q", got, url)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, identity := readIdentity(t, conn); identity != "sandbox-1" {
		t.Fatalf("pool saw client %q, want sandbox-1", identity)
	}
}

func TestNewDialerURLs(t *testing.T) {
	material := poolMaterial(t).Clients["sandbox-1"]
	tests := []struct {
		name       string
		url        string
		serverName string
		wantErr    string
		wantServer string
	}{
		{name: "vsock with server name", url: "vsock://2:17080", serverName: poolServerName, wantServer: "https://" + poolServerName},
		{name: "unix with server name", url: "unix:///run/discobox/pool.sock", serverName: poolServerName, wantServer: "https://" + poolServerName},
		{name: "vsock names no host", url: "vsock://2:17080", wantErr: "server name is required"},
		{name: "unix names no host", url: "unix:///run/discobox/pool.sock", wantErr: "server name is required"},
		{name: "plaintext refused", url: "http://discobox-pool-proxy:17080", wantErr: "must be https, vsock, or unix"},
		{name: "https needs a port", url: "https://discobox-pool-proxy", wantErr: "host and port"},
		{name: "no scheme", url: "discobox-pool-proxy:17080", wantErr: "unsupported endpoint scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer, err := bridge.NewDialer(dialConfig(material, tt.url, tt.serverName))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewDialer(%q) error = %v, want one containing %q", tt.url, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewDialer(%q): %v", tt.url, err)
			}
			if got := dialer.ServerURL(); got != tt.wantServer {
				t.Fatalf("ServerURL = %q, want %q", got, tt.wantServer)
			}
		})
	}
}
