// Package dnsstub resolves the sandbox's external names through its pool, over
// the sandbox's mTLS identity.
//
// A sandbox sits only on its pool's internal network, where Docker's embedded
// resolver answers container names but forwards nothing else. The stub runs in
// the sandbox's proxy bridge, which reaches the pool with the same keypair for
// its egress. The container is
// created with a link-local DNS server (sandboxconfig.SandboxDNSAddress), which
// the egress bridge config names as the stub's listener; this stub claims that
// address on the sandbox's own loopback, so Docker's resolver
// hands it every name it cannot answer, and the query never crosses the network
// as plain DNS. The stub carries each one to the pool as DNS over TLS
// (RFC 7858), presenting the sandbox's client certificate and verifying the
// pool's: another sandbox on the network can neither read nor answer it. It
// dials through bridge.Dialer like everything else in the sandbox that reaches
// the pool, so a pool behind an edge is reached through a tunnel
// (ADR 26-10-23-719 §5).
//
// The sandbox's nested Docker names the same address as its containers' name
// server (the image's daemon.json), so they reach the stub across its bridges.
package dnsstub

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/discobox-ai/discobox/proxy/bridge"
)

const (
	// DefaultBridgeConfigPath is the intake-rendered bridge config that names the
	// stub's address, the pool's DNS server, and the keypair to reach it with.
	DefaultBridgeConfigPath = "/etc/discobox/proxy/bridge.json"

	// queryTimeout bounds one exchange with the pool, dial included. Docker's
	// embedded resolver gives up on a forward before this.
	queryTimeout = 4 * time.Second
	// maxConns is how many connections to the pool the stub holds open, busy
	// or idle between queries. It stays under the pool's per-sandbox admission
	// (dnsforward's maxConnsPerClient), with room for a connection the pool
	// has not yet counted closed; a query that finds none free waits for one
	// rather than dialing past the pool's limit and failing.
	maxConns = 8
	// maxInFlight bounds concurrent queries, so a burst costs a bounded number
	// of goroutines and connections.
	maxInFlight = 64
	// maxMessage is the largest DNS message.
	maxMessage = 65535
	// minUDPSize is what a client that advertises nothing can receive over UDP.
	minUDPSize = 512
)

// Config is the stub's settings, which LoadConfig reads from the egress
// bridge config (bridge.File).
type Config struct {
	// ListenAddress is where the stub listens: the DNS server the sandbox
	// container was created with.
	ListenAddress netip.AddrPort
	// Pool is how the stub reaches the pool's DNS-over-TLS server, as the
	// bridge config gives it (File.Dial). Its URL is a wire URL as every
	// other pool endpoint is; a bare host:port, which a bootstrap from before
	// that names, is TCP: https://host:port.
	Pool bridge.DialConfig
}

// ErrNoDNS reports a bridge config from a pool that serves no DNS.
var ErrNoDNS = errors.New("the pool serves its sandboxes no DNS")

// LoadConfig reads the stub's settings from the egress bridge config.
func LoadConfig(path string) (Config, error) {
	file, err := bridge.LoadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Pool: file.Dial(file.DNSServer)}
	if file.DNSListenAddress != "" {
		if cfg.ListenAddress, err = netip.ParseAddrPort(file.DNSListenAddress); err != nil {
			return Config{}, fmt.Errorf("%s dnsListenAddress: %w", path, err)
		}
	}
	if cfg.Pool.URL == "" && !cfg.ListenAddress.IsValid() {
		return Config{}, ErrNoDNS
	}
	if cfg.Pool.URL == "" || !cfg.ListenAddress.IsValid() {
		return Config{}, fmt.Errorf("%s names only one of dnsServer and dnsListenAddress", path)
	}
	return cfg, nil
}

// Dialer reaches the pool's DNS server with the sandbox's keypair, read again
// whenever the intake replaces it, so a renewed certificate reaches the stub's
// next connection.
func (c Config) Dialer() (*bridge.Dialer, error) {
	pool := c.Pool
	if !strings.Contains(pool.URL, "://") {
		pool.URL = "https://" + pool.URL
	}
	return bridge.NewDialer(pool)
}

// ClaimAddress puts addr on the sandbox's loopback, so the stub can listen on
// it and Docker's embedded resolver, forwarding from inside the sandbox's
// network namespace, delivers to it locally. The sandbox's nested Docker names
// the same address as its containers' DNS server, so their queries arrive on
// one of its bridges and are answered too.
//
// A local address answers on every interface, so it also drops whatever is
// sent to addr on the network the box is on (pooledInterfaces): every sandbox
// of a pool shares that network, and a neighbor routing addr through this
// sandbox would otherwise have its queries asked of the pool under this
// sandbox's certificate. Both steps are idempotent across restarts.
//
// It never touches the box's resolver: in a container Docker's embedded one
// forwards here, and on a machine the provider's stays the box's own, with
// the stub beside it for the nested Docker (ADR 26-10-16-423 §9).
func ClaimAddress(ctx context.Context, addr netip.Addr) error {
	bits, iptables := 32, "iptables"
	if addr.Is6() {
		bits, iptables = 128, "ip6tables"
	}
	prefix := netip.PrefixFrom(addr, bits).String()
	if err := run(ctx, "ip", "address", "replace", prefix, "dev", "lo"); err != nil {
		return fmt.Errorf("claim %s on lo: %w", addr, err)
	}
	for _, iface := range pooledInterfaces {
		// -w waits for the xtables lock rather than failing when something
		// else in the sandbox, a starting dockerd say, holds it.
		rule := []string{"INPUT", "-d", prefix, "-i", iface, "-j", "DROP"}
		if run(ctx, iptables, append([]string{"-w", "-C"}, rule...)...) == nil {
			continue
		}
		if err := run(ctx, iptables, append([]string{"-w", "-I"}, rule...)...); err != nil {
			return fmt.Errorf("keep %s off the pool's network: %w", addr, err)
		}
	}
	return nil
}

// pooledInterfaces match the interfaces a box reaches its pool's network on:
// eth+ is every one Docker gives a sandbox container (eth0 on its pool's
// network, and any further network it is attached to) and a VM guest whose
// kernel names its NICs the classic way; en+ is every one a machine's
// systemd names predictably (ens3, enp0s1, eno1), which is how a provider's
// Ubuntu machine names its own (ADR 26-10-16-423 §9). The rest are lo, the
// bridges the sandbox's own nested Docker made, which may carry any name,
// and whatever the sandbox's root adds itself (a VPN's tun, say), whose peers
// it chose and could as well forward queries for (ADR 26-10-15-760 §2).
var pooledInterfaces = []string{"eth+", "en+"}

func run(ctx context.Context, name string, args ...string) error {
	//nolint:gosec // G204: fixed binaries (ip, iptables, ip6tables) with arguments built from a netip.Prefix and constants.
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Run serves the stub cfg describes until ctx ends: it loads the sandbox's
// credentials, claims the address, and answers there. It returns early only
// when one of those fails.
func Run(ctx context.Context, logger *slog.Logger, cfg Config) error {
	dialer, err := cfg.Dialer()
	if err != nil {
		return err
	}
	stub := New(logger, dialer)
	if err := ClaimAddress(ctx, cfg.ListenAddress.Addr()); err != nil {
		return err
	}
	var listenConfig net.ListenConfig
	udp, err := listenConfig.ListenPacket(ctx, "udp", cfg.ListenAddress.String())
	if err != nil {
		return err
	}
	tcp, err := listenConfig.Listen(ctx, "tcp", cfg.ListenAddress.String())
	if err != nil {
		_ = udp.Close()
		return err
	}
	stub.logger.Info("sandbox dns stub serving", "addr", cfg.ListenAddress, "pool", dialer.URL())
	stub.Serve(ctx, udp, tcp)
	return nil
}

// Stub answers DNS on the sandbox side and asks the pool.
type Stub struct {
	logger *slog.Logger
	// dialer reaches the pool, presenting the sandbox's certificate as it is
	// at each dial. A connection already open keeps the certificate it was
	// opened with; it was valid when the pool verified it.
	dialer *bridge.Dialer
	// ownNames are the pool's name and the name its transport dials (an
	// edge's, for a tunnel). A query for either reaching the stub cannot be
	// asked of the pool, since dialing it would mean resolving that same name
	// through the stub again: for the pool's name it means Docker's resolver
	// could not answer it, because the pool is not on the network.
	ownNames []string

	// idle holds open connections between queries, and open one token per
	// connection, busy or idle: a query takes an idle connection, or a token
	// to dial one, or waits for either.
	idle chan *tls.Conn
	open chan struct{}

	slots chan struct{}
}

// New returns a stub that asks the pool dialer reaches.
func New(logger *slog.Logger, dialer *bridge.Dialer) *Stub {
	if logger == nil {
		logger = slog.Default()
	}
	var names []string
	for _, name := range []string{dialer.ServerName(), dialer.TransportHost()} {
		if name = strings.ToLower(strings.TrimSuffix(name, ".")); name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return &Stub{
		logger:   logger,
		dialer:   dialer,
		ownNames: names,
		idle:     make(chan *tls.Conn, maxConns),
		open:     make(chan struct{}, maxConns),
		slots:    make(chan struct{}, maxInFlight),
	}
}

// Serve answers on udp and tcp until ctx ends, then closes them and every
// pool connection.
func (s *Stub) Serve(ctx context.Context, udp net.PacketConn, tcp net.Listener) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.serveUDP(ctx, udp) }()
	go func() { defer wg.Done(); s.serveTCP(ctx, tcp) }()
	<-ctx.Done()
	_ = udp.Close()
	_ = tcp.Close()
	wg.Wait()
	for {
		select {
		case conn := <-s.idle:
			s.discard(conn)
		default:
			return
		}
	}
}

func (s *Stub) serveUDP(ctx context.Context, udp net.PacketConn) {
	for {
		buf := make([]byte, maxMessage)
		n, client, err := udp.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("sandbox dns read failed", "error", err)
			sleep(ctx, 100*time.Millisecond)
			continue
		}
		select {
		case s.slots <- struct{}{}:
		default:
			continue // dropped; the client retries a lost datagram
		}
		go func() {
			defer func() { <-s.slots }()
			answer := s.answer(ctx, buf[:n])
			if answer == nil {
				return
			}
			_, _ = udp.WriteTo(fitUDP(buf[:n], answer), client)
		}()
	}
}

func (s *Stub) serveTCP(ctx context.Context, tcp net.Listener) {
	for {
		conn, err := tcp.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("sandbox dns accept failed", "error", err)
			sleep(ctx, 100*time.Millisecond)
			continue
		}
		select {
		case s.slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		go func() {
			defer func() { <-s.slots }()
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(2 * queryTimeout))
			query, err := readMessage(conn)
			if err != nil {
				return
			}
			if answer := s.answer(ctx, query); answer != nil {
				_ = writeMessage(conn, answer)
			}
		}()
	}
}

// answer is the reply to query, or nil for a message not worth one.
func (s *Stub) answer(ctx context.Context, query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		return nil
	}
	if slices.Contains(s.ownNames, strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))) {
		return failure(header, question)
	}
	answer, err := s.exchange(ctx, query)
	if err != nil {
		s.logger.Debug("sandbox dns exchange failed", "name", question.Name.String(), "error", err)
		return failure(header, question)
	}
	return answer
}

// exchange asks the pool on an open connection, waiting for one if all are
// busy. A connection that was idle may have been closed by the pool meanwhile,
// which surfaces only on use, so a failure on one is retried once on a fresh
// connection.
func (s *Stub) exchange(ctx context.Context, query []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		conn, reused, err := s.take(ctx)
		if err != nil {
			return nil, err
		}
		answer, err := roundTrip(conn, query)
		if err == nil {
			s.idle <- conn
			return answer, nil
		}
		s.discard(conn)
		if !reused || attempt > 0 {
			return nil, err
		}
	}
}

// take returns an idle connection, or dials one when fewer than maxConns are
// open, or waits for either until ctx ends.
func (s *Stub) take(ctx context.Context) (conn *tls.Conn, reused bool, err error) {
	select {
	case conn := <-s.idle:
		return conn, true, nil
	default:
	}
	select {
	case conn := <-s.idle:
		return conn, true, nil
	case s.open <- struct{}{}:
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
	conn, err = s.dialer.Dial(ctx)
	if err != nil {
		<-s.open
		return nil, false, err
	}
	return conn, false, nil
}

// discard closes a connection and returns its token.
func (s *Stub) discard(conn *tls.Conn) {
	_ = conn.Close()
	<-s.open
}

func roundTrip(conn *tls.Conn, query []byte) ([]byte, error) {
	_ = conn.SetDeadline(time.Now().Add(queryTimeout))
	if err := writeMessage(conn, query); err != nil {
		return nil, err
	}
	answer, err := readMessage(conn)
	if err != nil {
		return nil, err
	}
	if len(answer) < 2 || answer[0] != query[0] || answer[1] != query[1] {
		return nil, errors.New("pool answered a different query")
	}
	return answer, nil
}

// failure is a SERVFAIL for the query: a resolver moves on from it at once,
// where silence would make it wait out its timeout.
func failure(header dnsmessage.Header, question dnsmessage.Question) []byte {
	return reply(header, question, dnsmessage.RCodeServerFailure, false)
}

// fitUDP returns answer as it can be sent over UDP to the client that asked
// query: whole if it fits what the client advertised, otherwise the header and
// question alone with TC set, which sends the client back over TCP.
func fitUDP(query, answer []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return answer
	}
	question, err := parser.Question()
	if err != nil {
		return answer
	}
	limit := minUDPSize
	if err := parser.SkipAllQuestions(); err == nil {
		if err := parser.SkipAllAnswers(); err == nil {
			if err := parser.SkipAllAuthorities(); err == nil {
				for {
					rh, err := parser.AdditionalHeader()
					if err != nil {
						break
					}
					if rh.Type == dnsmessage.TypeOPT {
						limit = max(limit, int(rh.Class))
					}
					if err := parser.SkipAdditional(); err != nil {
						break
					}
				}
			}
		}
	}
	if len(answer) <= limit {
		return answer
	}
	rcode := dnsmessage.RCodeSuccess
	var answerParser dnsmessage.Parser
	if h, err := answerParser.Start(answer); err == nil {
		rcode = h.RCode
	}
	return reply(header, question, rcode, true)
}

func reply(header dnsmessage.Header, question dnsmessage.Question, rcode dnsmessage.RCode, truncated bool) []byte {
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		OpCode:             header.OpCode,
		RecursionDesired:   header.RecursionDesired,
		RecursionAvailable: true,
		Truncated:          truncated,
		RCode:              rcode,
	})
	if err := builder.StartQuestions(); err != nil {
		return nil
	}
	if err := builder.Question(question); err != nil {
		return nil
	}
	message, err := builder.Finish()
	if err != nil {
		return nil
	}
	return message
}

// readMessage and writeMessage frame one DNS message the way DNS over TCP, and
// so DNS over TLS, does: a two-byte big-endian length, then the message.
func readMessage(r io.Reader) ([]byte, error) {
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return nil, err
	}
	message := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(r, message); err != nil {
		return nil, err
	}
	return message, nil
}

func writeMessage(w io.Writer, message []byte) error {
	if len(message) > maxMessage {
		return fmt.Errorf("dns message of %d bytes exceeds %d", len(message), maxMessage)
	}
	_, err := w.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...))
	return err
}

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
