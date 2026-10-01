package ports

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"
)

// TestPlatformScannerSeesOwnListeners runs the real lsof against sockets this
// test opens, as the user running it.
func TestPlatformScannerSeesOwnListeners(t *testing.T) {
	ctx := context.Background()
	var lc net.ListenConfig
	tcp, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	// A UDP server binds a port of its own choosing; one below the ephemeral
	// range is what keeps it from looking like a client.
	var udp net.PacketConn
	for port := 20000; port < 20100 && udp == nil; port++ {
		udp, _ = lc.ListenPacket(ctx, "udp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	if udp == nil {
		t.Fatal("no UDP port below the ephemeral range was free")
	}
	defer udp.Close()

	got, err := platformScanner().scan(ctx, int64(os.Getuid()))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	tcpPort := tcp.Addr().(*net.TCPAddr).Port
	udpPort := udp.LocalAddr().(*net.UDPAddr).Port
	var sawTCP, sawUDP bool
	for _, entry := range got {
		switch {
		case entry.Network == networkTCP && entry.Port == tcpPort:
			sawTCP = entry.Addr.String() == "127.0.0.1" && entry.Socket != 0
		case entry.Network == networkUDP && entry.Port == udpPort:
			sawUDP = entry.Addr.String() == "127.0.0.1"
		}
	}
	if !sawTCP || !sawUDP {
		t.Fatalf("scan = %+v, want TCP 127.0.0.1:%d and UDP 127.0.0.1:%d", got, tcpPort, udpPort)
	}
}

func TestSysctlEphemeralRange(t *testing.T) {
	got := sysctlEphemeralRange()
	if got.low < 1024 || got.high > 65535 || got.low > got.high {
		t.Fatalf("ephemeral range = %+v, want a range of unprivileged ports", got)
	}
}
