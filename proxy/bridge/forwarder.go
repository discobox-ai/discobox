// Package bridge implements the sandbox-local forwarding proxy that accepts
// plaintext proxy traffic inside a sandbox and forwards it to the pool proxy
// over mTLS, except HTTP proxy requests for the sandbox's own networks, which
// it connects directly. The pool agent's build forwarder uses it the same way inside a
// build's network namespace. It is intentionally dependency-light so the
// sandbox-agent binary can embed it without importing the full pool proxy
// stack.
//
// Its Dialer is how anything in a sandbox reaches a pool service: the URL's
// scheme picks the transport through wire, and the sandbox's client
// certificate rides on top of whichever one it is.
package bridge

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/discobox-ai/discobox/proxy/bridge"

// Forwarder forwards sandbox-local plaintext proxy traffic to the pool proxy
// over mTLS. It is protocol agnostic, so HTTP and SOCKS traffic both flow
// through the pool proxy's protocol detector.
type Forwarder struct {
	ctx           context.Context
	listenAddress string
	worker        *Dialer
	localSubnets  func() []string

	listener net.Listener
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
}

// Config controls a sandbox-local forwarder.
type Config struct {
	ListenAddress string
	// WorkerProxyURL is the pool proxy, in DialConfig.URL's vocabulary.
	WorkerProxyURL string
	// ServerName is the name the pool proxy's certificate is verified as; see
	// DialConfig.ServerName.
	ServerName     string
	MTLSCAPath     string
	ClientCertPath string
	ClientKeyPath  string

	// LocalSubnets lists, as CIDRs, the networks this host is directly
	// connected to. A connection whose first request is an HTTP proxy request
	// for an IP address inside one is connected directly rather than sent to the pool proxy, which cannot
	// reach a sandbox's own networks. NO_PROXY names the same subnets, but
	// not every client can match a CIDR there: Node's built-in fetch matches
	// only exact hosts and name suffixes. Nil forwards everything.
	//
	// It is called when such a request arrives, never cached: the nested
	// Docker bridge and user-created networks appear after boot.
	LocalSubnets func() []string
}

// New creates a sandbox-local forwarder.
func New(ctx context.Context, cfg Config) (*Forwarder, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:0"
	}
	worker, err := NewDialer(DialConfig{
		URL:            cfg.WorkerProxyURL,
		ServerName:     cfg.ServerName,
		MTLSCAPath:     cfg.MTLSCAPath,
		ClientCertPath: cfg.ClientCertPath,
		ClientKeyPath:  cfg.ClientKeyPath,
	})
	if err != nil {
		return nil, err
	}
	return &Forwarder{
		ctx:           ctx,
		listenAddress: cfg.ListenAddress,
		worker:        worker,
		localSubnets:  cfg.LocalSubnets,
		conns:         map[net.Conn]struct{}{},
		closed:        make(chan struct{}),
	}, nil
}

// ListenAndServe starts the local forwarding listener.
func (f *Forwarder) ListenAndServe() error {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(f.ctx, "tcp", f.listenAddress)
	if err != nil {
		return err
	}
	return f.Serve(listener)
}

// Serve runs the forwarder's accept loop on an already-open listener, such as
// one systemd passed via socket activation (see
// github.com/coreos/go-systemd/v22/activation). ListenAndServe is Serve over a
// listener this Forwarder dials itself.
func (f *Forwarder) Serve(listener net.Listener) error {
	f.listener = listener
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-f.closed:
				return nil
			default:
				return err
			}
		}
		f.wg.Add(1)
		go f.forward(conn)
	}
}

// Close stops the forwarder and closes active connections.
func (f *Forwarder) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	if f.listener != nil {
		_ = f.listener.Close()
	}
	f.connMu.Lock()
	for conn := range f.conns {
		_ = conn.Close()
	}
	f.connMu.Unlock()
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	return nil
}

// Addr returns the listener address after ListenAndServe starts.
func (f *Forwarder) Addr() net.Addr {
	if f == nil || f.listener == nil {
		return nil
	}
	return f.listener.Addr()
}

func (f *Forwarder) forward(local net.Conn) {
	ctx, span := otel.Tracer(tracerName).Start(f.ctx, "proxy.bridge.connection",
		trace.WithAttributes(attribute.String("proxy.worker.address", f.worker.URL())),
	)
	defer span.End()
	defer f.wg.Done()
	f.trackConn(local)
	defer f.untrackConn(local)

	client := bufio.NewReaderSize(local, maxRequestLine)
	if target, connect, ok := f.localTarget(client); ok {
		span.SetAttributes(attribute.String("proxy.bridge.direct", target))
		f.direct(ctx, local, client, target, connect)
		return
	}

	worker, err := f.worker.Dial(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		// Log as well as trace: closing here surfaces to the sandbox process as
		// a bare connection reset, so without a log line every proxy failure
		// (expired or misnamed certificates, an unreachable pool) is invisible
		// from inside the sandbox and from the pool's journal alike.
		slog.Error("sandbox proxy bridge could not reach the pool proxy",
			"worker", f.worker.URL(), "error", err)
		_ = local.Close()
		return
	}
	f.trackConn(worker)
	defer f.untrackConn(worker)

	splice(local, worker, func() error {
		_, err := io.Copy(worker, client)
		return err
	})
}

// splice copies remote's bytes to local while toRemote sends local's the other
// way, closing each side when its direction ends, until both have.
func splice(local, remote net.Conn, toRemote func() error) {
	var copyWg sync.WaitGroup
	copyWg.Add(2)
	go func() {
		defer copyWg.Done()
		_ = toRemote()
		_ = remote.Close()
	}()
	go func() {
		defer copyWg.Done()
		_, _ = io.Copy(local, remote)
		_ = local.Close()
	}()
	copyWg.Wait()
}

func (f *Forwarder) trackConn(conn net.Conn) {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	f.conns[conn] = struct{}{}
}

func (f *Forwarder) untrackConn(conn net.Conn) {
	_ = conn.Close()
	f.connMu.Lock()
	defer f.connMu.Unlock()
	delete(f.conns, conn)
}
