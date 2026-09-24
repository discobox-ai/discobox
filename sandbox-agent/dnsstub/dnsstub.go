// Package dnsstub resolves the sandbox's external names through its pool, over
// the sandbox's mTLS identity.
//
// A sandbox sits only on its pool's internal network, where Docker's embedded
// resolver answers container names but forwards nothing else. The stub runs in
// the sandbox's proxy bridge, which reaches the pool with the same keypair for
// its egress. The container is
// created with a link-local DNS server the pool staged in bridge.json; this
// stub claims that address on the sandbox's own loopback, so Docker's resolver
// hands it every name it cannot answer, and the query never crosses the network
// as plain DNS. The stub carries each one to the pool as DNS over TLS
// (RFC 7858), presenting the sandbox's client certificate and verifying the
// pool's: another sandbox on the network can neither read nor answer it.
package dnsstub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	// DefaultBridgeConfigPath is the pool-staged proxy material that names the
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

// Config is the subset of bridge.json the stub needs.
type Config struct {
	// ListenAddress is where the stub listens: the DNS server the sandbox
	// container was created with.
	ListenAddress netip.AddrPort `json:"dnsListenAddress"`
	// Server is the pool's DNS-over-TLS endpoint, host:port. Its host is also
	// the name the pool's certificate is verified for.
	Server         string `json:"dnsServer"`
	MTLSCAPath     string `json:"mtlsCaPath"`
	ClientCertPath string `json:"clientCertPath"`
	ClientKeyPath  string `json:"clientKeyPath"`
}

// ErrNoDNS reports a bridge config from a pool that serves no DNS.
var ErrNoDNS = errors.New("the pool serves its sandboxes no DNS")

// LoadConfig reads the stub's settings from a pool-staged bridge config.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Server == "" && !cfg.ListenAddress.IsValid() {
		return Config{}, ErrNoDNS
	}
	if cfg.Server == "" || !cfg.ListenAddress.IsValid() {
		return Config{}, fmt.Errorf("%s names only one of dnsServer and dnsListenAddress", path)
	}
	return cfg, nil
}

// TLSConfig is the client side of the pool's mTLS: the sandbox's keypair, and
// the pool CA verifying the pool's certificate for the server's host.
func (c Config) TLSConfig() (*tls.Config, error) {
	host, _, err := net.SplitHostPort(c.Server)
	if err != nil {
		return nil, fmt.Errorf("dns server %q: %w", c.Server, err)
	}
	caPEM, err := os.ReadFile(c.MTLSCAPath)
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse mTLS CA")
	}
	pair, err := tls.LoadX509KeyPair(c.ClientCertPath, c.ClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	return &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}, ServerName: host, MinVersion: tls.VersionTLS12}, nil
}

// ClaimAddress puts addr on the sandbox's loopback, so the stub can listen on
// it and Docker's embedded resolver, forwarding from inside the sandbox's
// network namespace, delivers to it locally.
//
// A local address answers on every interface, so it also drops whatever is
// sent to addr from off the sandbox: a neighbor routing addr through this
// sandbox would otherwise have its queries asked of the pool under this
// sandbox's certificate. Both steps are idempotent across restarts.
func ClaimAddress(ctx context.Context, addr netip.Addr) error {
	bits, iptables := 32, "iptables"
	if addr.Is6() {
		bits, iptables = 128, "ip6tables"
	}
	prefix := netip.PrefixFrom(addr, bits).String()
	if err := run(ctx, "ip", "address", "replace", prefix, "dev", "lo"); err != nil {
		return fmt.Errorf("claim %s on lo: %w", addr, err)
	}
	// -w waits for the xtables lock rather than failing when something else in
	// the sandbox, a starting dockerd say, holds it.
	rule := []string{"INPUT", "-d", prefix, "!", "-i", "lo", "-j", "DROP"}
	if run(ctx, iptables, append([]string{"-w", "-C"}, rule...)...) == nil {
		return nil
	}
	if err := run(ctx, iptables, append([]string{"-w", "-I"}, rule...)...); err != nil {
		return fmt.Errorf("keep %s local: %w", addr, err)
	}
	return nil
}

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
	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		return err
	}
	stub, err := New(logger, cfg.Server, tlsConfig)
	if err != nil {
		return err
	}
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
	stub.logger.Info("sandbox dns stub serving", "addr", cfg.ListenAddress, "pool", cfg.Server)
	stub.Serve(ctx, udp, tcp)
	return nil
}

// Stub answers DNS on the sandbox side and asks the pool.
type Stub struct {
	logger *slog.Logger
	server string
	tls    *tls.Config
	// poolHost is the pool's name. A query for it reaching the stub means
	// Docker's resolver could not answer it — the pool is not on the network —
	// and asking the pool would mean resolving that same name to dial it.
	poolHost string

	// idle holds open connections between queries, and open one token per
	// connection, busy or idle: a query takes an idle connection, or a token
	// to dial one, or waits for either.
	idle chan *tls.Conn
	open chan struct{}

	slots chan struct{}
}

// New returns a stub that asks server, a host:port, over tlsConfig.
func New(logger *slog.Logger, server string, tlsConfig *tls.Config) (*Stub, error) {
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return nil, fmt.Errorf("dns server %q: %w", server, err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Stub{
		logger:   logger,
		server:   server,
		tls:      tlsConfig,
		poolHost: strings.ToLower(strings.TrimSuffix(host, ".")),
		idle:     make(chan *tls.Conn, maxConns),
		open:     make(chan struct{}, maxConns),
		slots:    make(chan struct{}, maxInFlight),
	}, nil
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
	if strings.ToLower(strings.TrimSuffix(question.Name.String(), ".")) == s.poolHost {
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
	dialer := tls.Dialer{Config: s.tls}
	raw, err := dialer.DialContext(ctx, "tcp", s.server)
	if err != nil {
		<-s.open
		return nil, false, err
	}
	conn, ok := raw.(*tls.Conn)
	if !ok {
		_ = raw.Close()
		<-s.open
		return nil, false, fmt.Errorf("dial %s: not a TLS connection", s.server)
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
