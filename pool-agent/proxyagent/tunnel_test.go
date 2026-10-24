package proxyagent

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/wstunnel"
)

func TestValidateTunnelURL(t *testing.T) {
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{"wss://pool.example.com", true},
		{"wss://pool.example.com:8443", true},
		{"wss://pool.example.com/", true},
		{"https://pool.example.com", false},
		{"ws://pool.example.com", false},
		{"wss://", false},
		{"wss://pool.example.com/tunnel", false},
		{"wss://pool.example.com?x=1", false},
		{"wss://user@pool.example.com", false},
	} {
		if err := ValidateTunnelURL(tc.value); (err == nil) != tc.ok {
			t.Errorf("ValidateTunnelURL(%q) = %v, want ok=%v", tc.value, err, tc.ok)
		}
	}
}

func TestConfiguredTunnelURL(t *testing.T) {
	t.Setenv(EnvTunnelURL, "")
	if got, err := ConfiguredTunnelURL(); got != "" || err != nil {
		t.Fatalf("unset = %q, %v; want none", got, err)
	}
	t.Setenv(EnvTunnelURL, " wss://pool.example.com/ ")
	if got, err := ConfiguredTunnelURL(); got != "wss://pool.example.com" || err != nil {
		t.Fatalf("set = %q, %v; want the trimmed URL", got, err)
	}
	t.Setenv(EnvTunnelURL, "https://pool.example.com")
	if _, err := ConfiguredTunnelURL(); err == nil {
		t.Fatal("an https tunnel URL was accepted")
	}
}

// The proxy unit serves tunnels only for a pool behind an edge, and learns that
// from its environment file, since systemd starts it with a clean one.
func TestUnitEnvironmentCarriesTheTunnelURLOnlyWhenSet(t *testing.T) {
	if content := unitEnvironment("/host", "", testProjectID, testPoolID, ""); strings.Contains(content, EnvTunnelURL) {
		t.Fatalf("unit environment names a tunnel for a pool behind none:\n%s", content)
	}
	content := unitEnvironment("/host", "", testProjectID, testPoolID, "wss://pool.example.com")
	if !strings.Contains(content, EnvTunnelURL+"=wss://pool.example.com\n") {
		t.Fatalf("unit environment is missing the tunnel URL:\n%s", content)
	}
}

// Every service a sandbox reaches has a tunnel path, spliced onto that
// service's own listener in this container.
func TestTunnelServicesSpliceOntoThePoolsOwnListeners(t *testing.T) {
	want := map[string]string{
		tunnelProxyPath:       "127.0.0.1:17080",
		tunnelCredentialsPath: "127.0.0.1:17083",
		tunnelDNSPath:         "127.0.0.1:17085",
	}
	if len(tunnelServices) != len(want) {
		t.Fatalf("tunnel services = %v, want %v", tunnelServices, want)
	}
	for path, target := range want {
		if tunnelServices[path] != target {
			t.Errorf("%s splices onto %q, want %q", path, tunnelServices[path], target)
		}
	}
}

// A stream through the tunnel reaches the service's listener byte for byte,
// and ending it on either side ends it on the other.
func TestSpliceTunnelStreamCarriesBothWays(t *testing.T) {
	var listenConfig net.ListenConfig
	service, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	sources := make(chan netip.Addr, 4)
	go func() {
		for {
			conn, err := service.Accept()
			if err != nil {
				return
			}
			sources <- conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr()
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := service.Addr().String()
	edge := httptest.NewTLSServer(wstunnel.Handler(ctx, func(stream net.Conn) {
		spliceTunnelStream(ctx, slog.Default(), stream, target)
	}))
	defer edge.Close()

	client := wstunnel.NewClient(strings.Replace(edge.URL, "https://", "wss://", 1)+tunnelProxyPath, edge.Client())
	for range 2 {
		stream, err := client.Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(stream, "ping"); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 4)
		if _, err := io.ReadFull(stream, got); err != nil || string(got) != "ping" {
			t.Fatalf("echo = %q, %v", got, err)
		}
		_ = stream.Close()
		// The DNS server exempts this source alone from its per-source cap.
		if source := <-sources; source != TunnelSource {
			t.Fatalf("tunnel spliced from %v, want %v", source, TunnelSource)
		}
	}
}
