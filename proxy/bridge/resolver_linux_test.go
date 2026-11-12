package bridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The wire format only records nameserver IPs, so this fixture needs port 53.
// It is exercised in the Linux sandbox and skipped on hosts that reserve it.
func TestProviderDialResolvesAgainAfterRecordChanges(t *testing.T) {
	var queries atomic.Int32
	for _, host := range []string{"127.253.53.1", "127.253.53.2"} {
		udp, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp", net.JoinHostPort(host, "53"))
		if err != nil {
			t.Skipf("DNS fixture needs a free unprivileged loopback port 53: %v", err)
		}
		t.Cleanup(func() { _ = udp.Close() })
		go func() {
			buf := make([]byte, 4096)
			for {
				n, addr, err := udp.ReadFrom(buf)
				if err != nil {
					return
				}
				var message dnsmessage.Message
				if message.Unpack(buf[:n]) != nil {
					continue
				}
				queries.Add(1)
				message.Response = true
				message.Authoritative = true
				for _, q := range message.Questions {
					if q.Type == dnsmessage.TypeAAAA {
						message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET, TTL: 1}, Body: &dnsmessage.AAAAResource{AAAA: [16]byte{15: 1}}})
					}
					if q.Type == dnsmessage.TypeA {
						message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1}, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}})
					}
				}
				out, _ := message.Pack()
				_, _ = udp.WriteTo(out, addr)
			}
		}()
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	path := filepath.Join(t.TempDir(), "record.json")
	dial := providerDialContext(path)
	for _, host := range []string{"127.253.53.1", "127.253.53.2"} {
		encoded, marshalErr := json.Marshal(ProviderResolver{Content: "nameserver " + host + "\n"})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		before := queries.Load()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		conn, err := dial(ctx, "tcp", net.JoinHostPort("pool.provider.invalid", port))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
		if queries.Load() <= before {
			t.Fatal("provider DNS not queried at next dial")
		}
	}
}
