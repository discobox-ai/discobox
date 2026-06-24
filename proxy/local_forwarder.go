package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// LocalForwarder forwards sandbox-local plaintext proxy traffic to the worker
// proxy over mTLS. It is protocol agnostic, so HTTP and SOCKS traffic both flow
// through the worker proxy's protocol detector.
type LocalForwarder struct {
	ctx           context.Context
	listenAddress string
	workerAddress string
	tlsConfig     *tls.Config

	listener net.Listener
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
}

// LocalForwarderConfig controls a sandbox-local forwarding proxy.
type LocalForwarderConfig struct {
	ListenAddress  string
	WorkerProxyURL string
	MTLSCAPath     string
	ClientCertPath string
	ClientKeyPath  string
}

// NewLocalForwarder creates a sandbox-local forwarder.
func NewLocalForwarder(ctx context.Context, cfg LocalForwarderConfig) (*LocalForwarder, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:0"
	}
	workerAddress, serverName, err := parseWorkerProxyAddress(cfg.WorkerProxyURL)
	if err != nil {
		return nil, err
	}
	caPEM, err := os.ReadFile(cfg.MTLSCAPath)
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse mTLS CA")
	}
	clientCert, err := tls.LoadX509KeyPair(cfg.ClientCertPath, cfg.ClientKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load client certificate: %w", err)
	}
	return &LocalForwarder{
		ctx:           ctx,
		listenAddress: cfg.ListenAddress,
		workerAddress: workerAddress,
		tlsConfig: &tls.Config{
			RootCAs:      caPool,
			Certificates: []tls.Certificate{clientCert},
			ServerName:   serverName,
			MinVersion:   tls.VersionTLS12,
		},
		conns:  map[net.Conn]struct{}{},
		closed: make(chan struct{}),
	}, nil
}

// ListenAndServe starts the local forwarding listener.
func (f *LocalForwarder) ListenAndServe() error {
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(f.ctx, "tcp", f.listenAddress)
	if err != nil {
		return err
	}
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
func (f *LocalForwarder) Close() error {
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
func (f *LocalForwarder) Addr() net.Addr {
	if f == nil || f.listener == nil {
		return nil
	}
	return f.listener.Addr()
}

func (f *LocalForwarder) forward(local net.Conn) {
	ctx, span := proxyTracer().Start(f.ctx, "proxy.local_forwarder.connection",
		trace.WithAttributes(attribute.String("proxy.worker.address", f.workerAddress)),
	)
	defer span.End()
	defer f.wg.Done()
	f.trackConn(local)
	defer f.untrackConn(local)

	dialer := tls.Dialer{NetDialer: &net.Dialer{}, Config: f.tlsConfig}
	worker, err := dialer.DialContext(ctx, "tcp", f.workerAddress)
	if err != nil {
		recordSpanError(span, err)
		_ = local.Close()
		return
	}
	f.trackConn(worker)
	defer f.untrackConn(worker)

	var copyWg sync.WaitGroup
	copyWg.Add(2)
	go func() {
		defer copyWg.Done()
		_, _ = io.Copy(worker, local)
		_ = worker.Close()
	}()
	go func() {
		defer copyWg.Done()
		_, _ = io.Copy(local, worker)
		_ = local.Close()
	}()
	copyWg.Wait()
}

func (f *LocalForwarder) trackConn(conn net.Conn) {
	f.connMu.Lock()
	defer f.connMu.Unlock()
	f.conns[conn] = struct{}{}
}

func (f *LocalForwarder) untrackConn(conn net.Conn) {
	_ = conn.Close()
	f.connMu.Lock()
	defer f.connMu.Unlock()
	delete(f.conns, conn)
}

func parseWorkerProxyAddress(raw string) (address string, serverName string, err error) {
	if raw == "" {
		return "", "", fmt.Errorf("worker proxy URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse worker proxy URL: %w", err)
	}
	if parsed.Scheme == "" {
		parsed, err = url.Parse("https://" + raw)
		if err != nil {
			return "", "", fmt.Errorf("parse worker proxy URL: %w", err)
		}
	}
	if parsed.Scheme != "https" {
		return "", "", fmt.Errorf("worker proxy URL must use https")
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("worker proxy URL host is required")
	}
	return parsed.Host, parsed.Hostname(), nil
}
