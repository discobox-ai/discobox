// Package dnsforward answers sandboxes' DNS over their mTLS channel to the
// pool.
//
// A sandbox sits only on its pool's internal network, where Docker's embedded
// DNS answers container names but forwards nothing else, so every external name
// fails. The sandbox runs a local stub (sandbox-agent's dnsstub) that carries
// what Docker cannot answer here as DNS over TLS (RFC 7858: two-byte
// length-prefixed messages on a TLS stream), authenticated both ways with the
// same certificates as the pool proxy. Plain DNS on that network would be
// answerable by any sandbox that claimed the pool's address; this cannot be.
//
// Each message is answered by the pool container's own resolver, which reaches
// the outside, and relayed as it came back: which names resolve is the
// upstream's business, exactly as it is for the pool itself. Each is also
// decoded enough to audit — the question, the outcome, the answers — into the
// proxy's trail beside the sandbox's HTTP (ADR 0148).
package dnsforward

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/discobox-ai/discobox/proxy"
)

const (
	// Port is the DNS port the upstream resolver answers on.
	Port = 53

	// queryTimeout bounds one exchange with the upstream.
	queryTimeout = 5 * time.Second
	// idleTimeout closes a connection with no query in flight. The sandbox
	// keeps a few open to spare each query a handshake, and redials on close.
	idleTimeout = 60 * time.Second
	// maxConns bounds connections across the pool and maxConnsPerSource those
	// from one address, both taken at accept, before the handshake, so a flood
	// — certified or not — costs this process a bounded number of descriptors;
	// it shares them with every sandbox's egress. maxConnsPerClient bounds one
	// sandbox's once its certificate names it, so no sandbox takes every slot
	// from the rest. The sandbox's stub keeps below it.
	maxConns          = 1024
	maxConnsPerSource = 32
	maxConnsPerClient = 16
	// maxRetryDelay caps the pause after a failed accept, as net/http's does:
	// an error that persists, such as running out of descriptors, must not
	// become a loop that spins a core and floods the log.
	maxRetryDelay = time.Second
	// maxMessage is the largest DNS message.
	maxMessage = 65535
)

// Server answers DNS-over-TLS connections from sandboxes.
type Server struct {
	logger   *slog.Logger
	upstream string
	audit    func(proxy.DNSAuditEvent)

	mu      sync.Mutex
	total   int
	sources map[netip.Addr]int
	clients map[string]int
}

// New returns a server that answers from upstream, a host:port, and hands
// every query it answered, or failed to, to audit. audit must not block.
func New(logger *slog.Logger, upstream string, audit func(proxy.DNSAuditEvent)) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{logger: logger, upstream: upstream, audit: audit, sources: map[netip.Addr]int{}, clients: map[string]int{}}
}

// Serve answers connections on listener, which must be a TLS listener that
// requires and verifies a client certificate: its common name is the sandbox
// the connection is counted against. It returns when ctx ends, closing the
// listener.
func (s *Server) Serve(ctx context.Context, listener net.Listener) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var retry backoff
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.logger.Warn("sandbox dns accept failed", "error", err)
			retry.wait(ctx)
			continue
		}
		retry.reset()
		source := remoteAddr(conn)
		if !s.admitSource(source) {
			// Refused rather than queued: the stub redials on a closed
			// connection, and a queue would hold the descriptor anyway.
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.releaseSource(source)
			s.serveConn(ctx, conn)
		}()
	}
}

func remoteAddr(conn net.Conn) netip.Addr {
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		return addr.AddrPort().Addr().Unmap()
	}
	return netip.Addr{}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		s.logger.Warn("sandbox dns connection is not TLS", "remote", conn.RemoteAddr())
		return
	}
	_ = tlsConn.SetDeadline(time.Now().Add(queryTimeout))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		s.logger.Debug("sandbox dns handshake failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}
	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return
	}
	cert := certs[0]
	client := cert.Subject.CommonName
	if !s.admitClient(client) {
		return
	}
	defer s.releaseClient(client)
	for {
		_ = tlsConn.SetReadDeadline(time.Now().Add(idleTimeout))
		query, err := readMessage(tlsConn)
		if err != nil {
			return
		}
		start := time.Now()
		answer, err := s.exchange(ctx, query)
		event := auditEvent(query, answer, err)
		event.Time = start.UTC()
		event.Duration = time.Since(start)
		event.ClientID = client
		event.ClientSubject = cert.Subject.String()
		event.ClientSerial = cert.SerialNumber.String()
		s.audit(event)
		if err != nil {
			s.logger.Debug("sandbox dns exchange failed", "sandbox", client, "error", err)
			return
		}
		_ = tlsConn.SetWriteDeadline(time.Now().Add(queryTimeout))
		if err := writeMessage(tlsConn, answer); err != nil {
			return
		}
	}
}

func (s *Server) admitSource(source netip.Addr) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxConns || s.sources[source] >= maxConnsPerSource {
		return false
	}
	s.total++
	s.sources[source]++
	return true
}

func (s *Server) releaseSource(source netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	if s.sources[source]--; s.sources[source] <= 0 {
		delete(s.sources, source)
	}
}

func (s *Server) admitClient(client string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[client] >= maxConnsPerClient {
		return false
	}
	s.clients[client]++
	return true
}

func (s *Server) releaseClient(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clients[client]--; s.clients[client] <= 0 {
		delete(s.clients, client)
	}
}

// exchange asks the upstream over UDP, and again over TCP when the answer
// comes back truncated: the stub is owed the whole answer, and trims it itself
// for a client that asked over UDP.
func (s *Server) exchange(ctx context.Context, query []byte) ([]byte, error) {
	answer, err := s.exchangeUDP(ctx, query)
	if err != nil {
		return nil, err
	}
	if !truncated(answer) {
		return answer, nil
	}
	return s.exchangeTCP(ctx, query)
}

func (s *Server) exchangeUDP(ctx context.Context, query []byte) ([]byte, error) {
	dialer := net.Dialer{Timeout: queryTimeout}
	conn, err := dialer.DialContext(ctx, "udp", s.upstream)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(queryTimeout))
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	answer := make([]byte, maxMessage)
	n, err := conn.Read(answer)
	if err != nil {
		return nil, err
	}
	return answer[:n], nil
}

func (s *Server) exchangeTCP(ctx context.Context, query []byte) ([]byte, error) {
	dialer := net.Dialer{Timeout: queryTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.upstream)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(queryTimeout))
	if err := writeMessage(conn, query); err != nil {
		return nil, err
	}
	return readMessage(conn)
}

// auditEvent is what the trail keeps of one exchange: the question, and the
// outcome — an rcode and the answers' data, or why there was no answer. A
// message that does not decode is still recorded, as that, since what a
// sandbox sent is the point of the trail.
func auditEvent(query, answer []byte, exchangeErr error) proxy.DNSAuditEvent {
	var event proxy.DNSAuditEvent
	var parser dnsmessage.Parser
	if _, err := parser.Start(query); err != nil {
		event.Error = "query does not decode: " + err.Error()
		return event
	}
	if question, err := parser.Question(); err == nil {
		event.Name = strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))
		event.Type = strings.TrimPrefix(question.Type.String(), "Type")
	}
	if exchangeErr != nil {
		event.Error = exchangeErr.Error()
		return event
	}
	header, err := parser.Start(answer)
	if err != nil {
		event.Error = "answer does not decode: " + err.Error()
		return event
	}
	event.RCode = rcodeName(header.RCode)
	if err := parser.SkipAllQuestions(); err != nil {
		return event
	}
	for {
		resource, err := parser.Answer()
		if err != nil {
			break
		}
		if data := answerData(resource.Body); data != "" {
			event.Answers = append(event.Answers, data)
		}
	}
	return event
}

// answerData is the part of an answer a reader looks for: the address, or the
// name an alias or pointer leads to. Records whose data is free text — TXT and
// the like — are left out rather than copied into the trail.
func answerData(body dnsmessage.ResourceBody) string {
	switch record := body.(type) {
	case *dnsmessage.AResource:
		return netip.AddrFrom4(record.A).String()
	case *dnsmessage.AAAAResource:
		return netip.AddrFrom16(record.AAAA).String()
	case *dnsmessage.CNAMEResource:
		return strings.TrimSuffix(record.CNAME.String(), ".")
	case *dnsmessage.PTRResource:
		return strings.TrimSuffix(record.PTR.String(), ".")
	case *dnsmessage.NSResource:
		return strings.TrimSuffix(record.NS.String(), ".")
	case *dnsmessage.MXResource:
		return strings.TrimSuffix(record.MX.String(), ".")
	case *dnsmessage.SRVResource:
		return fmt.Sprintf("%s:%d", strings.TrimSuffix(record.Target.String(), "."), record.Port)
	}
	return ""
}

// rcodeName is an rcode as DNS tools print it, which is how a reader searches
// for one.
func rcodeName(rcode dnsmessage.RCode) string {
	switch rcode {
	case dnsmessage.RCodeSuccess:
		return "NOERROR"
	case dnsmessage.RCodeFormatError:
		return "FORMERR"
	case dnsmessage.RCodeServerFailure:
		return "SERVFAIL"
	case dnsmessage.RCodeNameError:
		return "NXDOMAIN"
	case dnsmessage.RCodeNotImplemented:
		return "NOTIMP"
	case dnsmessage.RCodeRefused:
		return "REFUSED"
	}
	return fmt.Sprintf("RCODE%d", rcode)
}

// truncated reports the TC bit of a DNS message's header.
func truncated(message []byte) bool {
	return len(message) >= 3 && message[2]&0x02 != 0
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

// backoff is the pause between failed accepts: 5ms, doubling to maxRetryDelay,
// and back to nothing on the first success.
type backoff struct{ delay time.Duration }

func (b *backoff) wait(ctx context.Context) {
	if b.delay == 0 {
		b.delay = 5 * time.Millisecond
	} else {
		b.delay = min(2*b.delay, maxRetryDelay)
	}
	timer := time.NewTimer(b.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (b *backoff) reset() { b.delay = 0 }

// SystemUpstream is the first nameserver in resolvConf, as a host:port. In the
// pool container that is Docker's embedded resolver, which, unlike a
// sandbox's, forwards external names: the pool is also on a network with a
// route out.
func SystemUpstream(resolvConf string) (string, error) {
	file, err := os.Open(resolvConf)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil {
			return "", fmt.Errorf("%s: nameserver %q: %w", resolvConf, fields[1], err)
		}
		return netip.AddrPortFrom(addr.WithZone(""), Port).String(), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s names no nameserver", resolvConf)
}
