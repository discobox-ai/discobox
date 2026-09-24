package dnsstub

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/discobox-ai/discobox/proxy"
)

// fakePool is the pool's DNS-over-TLS endpoint: it answers every query with
// one A record, or with padding past a UDP datagram for names starting "big",
// after a pause for names starting "slow", and counts the connections it
// accepted.
type fakePool struct {
	addr     string
	accepted atomic.Int32
	mu       sync.Mutex
	conns    []net.Conn
	// open and peak count connections held at once, which is what the
	// pool's per-sandbox admission bounds.
	open, peak int
}

func (p *fakePool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
}

type pki struct {
	server *tls.Config
	config Config
}

// newPKI issues a pool server certificate for 127.0.0.1 and a sandbox client
// certificate from one CA, and the bridge config that names them.
func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	prepared, err := proxy.PrepareCertificates(proxy.PrepareOptions{Dir: dir, ServerHosts: []string{"127.0.0.1"}, ClientIDs: []string{"sbx_a"}})
	if err != nil {
		t.Fatal(err)
	}
	client := prepared.Clients["sbx_a"]
	cfg := Config{
		ListenAddress:  netip.MustParseAddrPort("127.0.0.1:0"),
		MTLSCAPath:     prepared.Bundle.MTLSCAPath,
		ClientCertPath: client.ClientCertPath,
		ClientKeyPath:  client.ClientKeyPath,
	}
	return pki{
		server: &tls.Config{
			Certificates: []tls.Certificate{prepared.Bundle.ServerCert},
			ClientCAs:    prepared.Bundle.ClientCAPool,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
		config: cfg,
	}
}

func startPool(t *testing.T, serverTLS *tls.Config) *fakePool {
	t.Helper()
	var config net.ListenConfig
	tcp, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(tcp, serverTLS)
	pool := &fakePool{addr: tcp.Addr().String()}
	t.Cleanup(func() { _ = listener.Close(); pool.closeAll() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			pool.accepted.Add(1)
			pool.mu.Lock()
			pool.conns = append(pool.conns, conn)
			pool.open++
			pool.peak = max(pool.peak, pool.open)
			pool.mu.Unlock()
			go func() {
				defer func() {
					_ = conn.Close()
					pool.mu.Lock()
					pool.open--
					pool.mu.Unlock()
				}()
				for {
					query, err := readMessage(conn)
					if err != nil {
						return
					}
					answer := poolAnswer(t, query)
					if bytes.Contains(query, []byte("slow")) {
						time.Sleep(200 * time.Millisecond)
					}
					if err := writeMessage(conn, answer); err != nil {
						return
					}
				}
			}()
		}
	}()
	return pool
}

func poolAnswer(t *testing.T, query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		t.Errorf("pool got an unparsable query: %v", err)
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		t.Errorf("pool got a query with no question: %v", err)
		return nil
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
	_ = builder.StartQuestions()
	_ = builder.Question(question)
	_ = builder.StartAnswers()
	rh := dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}
	records := 1
	if strings.HasPrefix(question.Name.String(), "big") {
		records = 60 // ~960 bytes of answers: past 512, inside 4096
	}
	for i := range records {
		_ = builder.AResource(rh, dnsmessage.AResource{A: [4]byte{192, 0, 2, byte(i)}})
	}
	answer, err := builder.Finish()
	if err != nil {
		t.Errorf("build answer: %v", err)
	}
	return answer
}

// startStub runs a stub against pool on loopback ports, the way the service
// does on the sandbox's DNS address.
func startStub(t *testing.T, p pki, poolAddr, poolHost string) (udp, tcp string) {
	t.Helper()
	p.config.Server = poolAddr
	tlsConfig, err := p.config.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(poolAddr)
	stub, err := New(nil, net.JoinHostPort(poolHost, port), tlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	// The stub dials the pool by name; in a sandbox Docker's resolver answers
	// it, here the test does.
	stub.server = poolAddr
	var config net.ListenConfig
	udpConn, err := config.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpListener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { stub.Serve(ctx, udpConn, tcpListener); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return udpConn.LocalAddr().String(), tcpListener.Addr().String()
}

func query(t *testing.T, id uint16, name string, udpSize int) []byte {
	t.Helper()
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	_ = builder.StartQuestions()
	_ = builder.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	if udpSize > 0 {
		_ = builder.StartAdditionals()
		var opt dnsmessage.ResourceHeader
		if err := opt.SetEDNS0(udpSize, dnsmessage.RCodeSuccess, false); err != nil {
			t.Fatal(err)
		}
		_ = builder.OPTResource(opt, dnsmessage.OPTResource{})
	}
	message, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func askUDP(t *testing.T, addr string, message []byte) dnsmessage.Message {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(message); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, maxMessage)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, buf[:n])
}

func askTCP(t *testing.T, addr string, message []byte) dnsmessage.Message {
	t.Helper()
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeMessage(conn, message); err != nil {
		t.Fatal(err)
	}
	answer, err := readMessage(conn)
	if err != nil {
		t.Fatal(err)
	}
	return parse(t, answer)
}

func parse(t *testing.T, data []byte) dnsmessage.Message {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(data); err != nil {
		t.Fatal(err)
	}
	return message
}

func TestStubAnswersFromThePoolOnOneConnection(t *testing.T) {
	p := newPKI(t)
	pool := startPool(t, p.server)
	udp, _ := startStub(t, p, pool.addr, "127.0.0.1")
	for id := range uint16(3) {
		answer := askUDP(t, udp, query(t, id, "example.com.", 0))
		if answer.ID != id || answer.RCode != dnsmessage.RCodeSuccess || len(answer.Answers) != 1 {
			t.Fatalf("answer %d = %+v", id, answer.Header)
		}
	}
	if got := pool.accepted.Load(); got != 1 {
		t.Fatalf("pool accepted %d connections for sequential queries, want 1", got)
	}
}

func TestStubRedialsWhenThePoolClosedAnIdleConnection(t *testing.T) {
	p := newPKI(t)
	pool := startPool(t, p.server)
	udp, _ := startStub(t, p, pool.addr, "127.0.0.1")
	askUDP(t, udp, query(t, 1, "example.com.", 0))
	pool.closeAll()
	if answer := askUDP(t, udp, query(t, 2, "example.com.", 0)); answer.RCode != dnsmessage.RCodeSuccess {
		t.Fatalf("after the pool closed the idle connection: rcode %v", answer.RCode)
	}
}

// A burst of more queries than the stub may hold connections waits for one to
// come free rather than dialing past the pool's per-sandbox admission and
// failing.
func TestStubQueuesABurstWithinItsConnections(t *testing.T) {
	p := newPKI(t)
	pool := startPool(t, p.server)
	udp, _ := startStub(t, p, pool.addr, "127.0.0.1")
	var wg sync.WaitGroup
	failures := make(chan string, 3*maxConns)
	for i := range 3 * maxConns {
		wg.Go(func() {
			answer := askUDP(t, udp, query(t, uint16(i), "slow.example.com.", 0))
			if answer.RCode != dnsmessage.RCodeSuccess {
				failures <- answer.RCode.String()
			}
		})
	}
	wg.Wait()
	close(failures)
	for rcode := range failures {
		t.Errorf("a query in the burst failed: %s", rcode)
	}
	// Held at once, not accepted in total: a connection that fails is
	// replaced, which is a new accept without ever holding more.
	pool.mu.Lock()
	peak := pool.peak
	pool.mu.Unlock()
	if peak > maxConns {
		t.Fatalf("pool held %d connections at once, want at most %d", peak, maxConns)
	}
}

func TestStubTruncatesForUDPAndAnswersInFullOverTCP(t *testing.T) {
	p := newPKI(t)
	pool := startPool(t, p.server)
	udp, tcp := startStub(t, p, pool.addr, "127.0.0.1")

	small := askUDP(t, udp, query(t, 1, "big.example.com.", 0))
	if !small.Truncated || len(small.Answers) != 0 {
		t.Fatalf("plain UDP: truncated=%v answers=%d, want a truncated empty answer", small.Truncated, len(small.Answers))
	}
	edns := askUDP(t, udp, query(t, 2, "big.example.com.", 4096))
	if edns.Truncated || len(edns.Answers) != 60 {
		t.Fatalf("UDP with EDNS 4096: truncated=%v answers=%d, want all 60", edns.Truncated, len(edns.Answers))
	}
	full := askTCP(t, tcp, query(t, 3, "big.example.com.", 0))
	if full.Truncated || len(full.Answers) != 60 {
		t.Fatalf("TCP: truncated=%v answers=%d, want all 60", full.Truncated, len(full.Answers))
	}
}

// Docker's resolver forwards the pool's own name only when it cannot answer it
// — when the pool is off the network — and dialing the pool would mean
// resolving that same name through this stub again.
func TestStubFailsThePoolsOwnNameWithoutAsking(t *testing.T) {
	p := newPKI(t)
	pool := startPool(t, p.server)
	udp, _ := startStub(t, p, pool.addr, "discobox-pool-proxy")
	answer := askUDP(t, udp, query(t, 7, "discobox-pool-proxy.", 0))
	if answer.ID != 7 || answer.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("answer = %+v, want SERVFAIL for id 7", answer.Header)
	}
	if got := pool.accepted.Load(); got != 0 {
		t.Fatalf("pool accepted %d connections for its own name, want 0", got)
	}
}

// A pool the stub cannot verify — any other host that answered on the pool's
// address — gets no query, and the client a SERVFAIL rather than its answer.
func TestStubRefusesAPoolItCannotVerify(t *testing.T) {
	p := newPKI(t)
	impostor := startPool(t, newPKI(t).server)
	udp, _ := startStub(t, p, impostor.addr, "127.0.0.1")
	if answer := askUDP(t, udp, query(t, 1, "example.com.", 0)); answer.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode = %v, want SERVFAIL", answer.RCode)
	}
}

func TestLoadConfig(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          Config
		wantErr       error
		anyErr        bool
	}{
		{
			name: "staged",
			content: `{"listenAddress":"127.0.0.1:17008","workerProxyUrl":"https://discobox-pool-proxy:17080",
				"dnsServer":"discobox-pool-proxy:17085","dnsListenAddress":"169.254.53.53:53",
				"mtlsCaPath":"/etc/discobox/proxy/mtls-ca.crt","clientCertPath":"/etc/discobox/proxy/client.crt","clientKeyPath":"/etc/discobox/proxy/client.key"}`,
			want: Config{
				ListenAddress:  netip.MustParseAddrPort("169.254.53.53:53"),
				Server:         "discobox-pool-proxy:17085",
				MTLSCAPath:     "/etc/discobox/proxy/mtls-ca.crt",
				ClientCertPath: "/etc/discobox/proxy/client.crt",
				ClientKeyPath:  "/etc/discobox/proxy/client.key",
			},
		},
		{name: "older pool", content: `{"listenAddress":"127.0.0.1:17008"}`, wantErr: ErrNoDNS},
		{name: "half", content: `{"dnsServer":"discobox-pool-proxy:17085"}`, anyErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bridge.json")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadConfig(path)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("LoadConfig error = %v, want %v", err, tc.wantErr)
				}
			case tc.anyErr:
				if err == nil {
					t.Fatalf("LoadConfig = %+v, want an error", got)
				}
			case err != nil:
				t.Fatal(err)
			case got != tc.want:
				t.Fatalf("LoadConfig = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TLSConfig verifies the pool for the dnsServer host, the name the pool's
// certificate is issued for.
func TestTLSConfigVerifiesTheServerHost(t *testing.T) {
	p := newPKI(t)
	p.config.Server = "discobox-pool-proxy:17085"
	cfg, err := p.config.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "discobox-pool-proxy" || len(cfg.Certificates) != 1 || cfg.RootCAs == nil {
		t.Fatalf("TLSConfig = ServerName %q, %d certificates, roots %v", cfg.ServerName, len(cfg.Certificates), cfg.RootCAs != nil)
	}
}
