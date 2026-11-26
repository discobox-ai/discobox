package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/pooloutbound"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/cache"
	"github.com/discobox-ai/discobox/proxy/internal/filter"
	"github.com/discobox-ai/discobox/proxy/internal/secrets"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Server is the mTLS pool proxy server.
type Server struct {
	ctx         context.Context
	cfg         Config
	certs       *CertificateBundle
	audit       *audit.Recorder
	cache       *cache.Cache
	filter      *filter.Filter
	http        *httpProxy
	socks       *socksProxy
	controlAuth *controlAuthenticator

	// connMu guards listener and conns, the closing of closed, and every
	// wg.Add that Close waits on: each happens under it after a check of
	// closed, so a Close racing a starting ListenAndServe or an Accept
	// either sees what it started, or stops it from starting at all.
	connMu   sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
	// draining is closed once Drain begins: the listener closing then is
	// not a failure to serve.
	draining chan struct{}
	// sweepStop ends the retention sweeper, which a drain stops before it
	// waits on the connections wg also counts.
	sweepStop chan struct{}
	sweepOnce sync.Once
}

// NewServer creates a pool-scoped proxy server. A nil resolver disables
// sentinel secret swapping; the resolver is a stable dependency preserved across
// ApplyConfig reloads.
func NewServer(ctx context.Context, cfg Config, certs *CertificateBundle, resolver SecretResolver) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg = normalizeConfig(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if certs == nil {
		var err error
		certs, err = LoadCertificateBundle(cfg.CertDir)
		if err != nil {
			return nil, err
		}
	}
	recorder, err := audit.Open(ctx, cfg.DatabaseDSN, cfg.Recording.QueueSize, cfg.Recording.Enabled)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	// A proxy that writes the database alone drops what its schema retired
	// before it serves; one with a SweepGate waits for the other to exit
	// (startRetentionSweeper).
	if cfg.Recording.SweepGate == nil {
		if err := recorder.DropRetiredColumns(ctx); err != nil {
			_ = recorder.Close()
			return nil, fmt.Errorf("audit: %w", err)
		}
	}
	recorder.ConfigureStreamSpool(cfg.Recording.StreamDir, cfg.Recording.StreamQueueSize)
	recorder.ConfigureBodySpool(cfg.Recording.BodyDir)
	recorder.ConfigureUnaudited(cfg.Recording.UnauditedClients)
	recorder.ConfigureSpoolBudget(audit.SpoolBudget{
		MaxBytes:   cfg.Recording.MaxSpoolBytes,
		MaxPercent: cfg.Recording.MaxSpoolPercent,
		HeadBytes:  cfg.Recording.BodyHeadBytes,
	})
	c, err := cache.New(cache.Config{
		Enabled:      cfg.Cache.Enabled,
		Dir:          cfg.Cache.Dir,
		MaxSizeBytes: cfg.Cache.MaxSizeBytes,
		Patterns:     cfg.Cache.Patterns,
		ContentAware: cfg.Cache.ContentAware,
	})
	if err != nil {
		return nil, fmt.Errorf("cache: %w", err)
	}
	controlAuth, err := newControlAuthenticator(cfg.Control)
	if err != nil {
		return nil, fmt.Errorf("control auth: %w", err)
	}
	flt := buildFilter(cfg)
	var swapper *secrets.Swapper
	if resolver != nil {
		swapper = secrets.New(resolver, swapperConfig(cfg))
		// A client's raw secrets never reach its trail (ADR 26-10-27-840).
		recorder.SetRedactor(func(clientID string) audit.Redactor {
			if set := swapper.Raw(clientID); set != nil {
				return set
			}
			return nil
		})
	}
	s := &Server{
		ctx:         ctx,
		cfg:         cfg,
		certs:       certs,
		audit:       recorder,
		cache:       c,
		filter:      flt,
		controlAuth: controlAuth,
		conns:       map[net.Conn]struct{}{},
		closed:      make(chan struct{}),
		draining:    make(chan struct{}),
		sweepStop:   make(chan struct{}),
	}
	s.http = newHTTPProxy(certs, s.filter, buildGatewayTables(cfg.Gateways), resolver, swapper, c, recorder)
	s.http.destinations.allowLoopback = cfg.AllowLoopbackDestinations
	if s.http.poolHosts, err = newPoolHosts(cfg.PoolHosts); err != nil {
		return nil, err
	}
	var upstream, httpsUpstream *url.URL
	var noProxy string
	if cfg.Outbound != nil {
		if err := pooloutbound.ValidateEndpoint(cfg.Outbound.HTTP); err != nil {
			return nil, err
		}
		if err := pooloutbound.ValidateEndpoint(cfg.Outbound.HTTPS); err != nil {
			return nil, err
		}
		if cfg.Outbound.HTTP != "" {
			upstream, _ = url.Parse(cfg.Outbound.HTTP)
		}
		if cfg.Outbound.HTTPS != "" {
			httpsUpstream, _ = url.Parse(cfg.Outbound.HTTPS)
		}
		noProxy = cfg.Outbound.NoProxy
	} else if cfg.UpstreamProxy == "" {
		inherited, resolveErr := pooloutbound.Resolve(pooloutbound.Intent{Mode: "inherit"}, os.LookupEnv)
		if resolveErr != nil {
			return nil, resolveErr
		}
		// Standalone consumes the host route itself rather than delivering
		// the logical endpoint into a backend's container or guest.
		inherited, resolveErr = inherited.ResolveLocalForwarder()
		if resolveErr != nil {
			return nil, resolveErr
		}
		if inherited.HTTP != "" {
			upstream, _ = url.Parse(inherited.HTTP)
		}
		if inherited.HTTPS != "" {
			httpsUpstream, _ = url.Parse(inherited.HTTPS)
		}
		noProxy = inherited.NoProxy
		if cfg.UpstreamNoProxy != "" {
			noProxy = cfg.UpstreamNoProxy
		}
	} else {
		upstream, err = upstreamProxyURL(cfg)
		if err != nil {
			return nil, err
		}
		httpsUpstream = upstream
		noProxy = upstreamNoProxy(cfg)
	}
	applyUpstreamProxy(s.http, upstream, httpsUpstream, noProxy)
	// After the upstream is applied: a pinned transport is a clone of the
	// proxy's own, and must leave the way it does.
	trusts, err := buildTrustTable(cfg.Trusts, s.http.proxy.Tr, nil)
	if err != nil {
		return nil, err
	}
	s.http.setTrusts(trusts)
	s.http.h2c = cleartextHTTP2Transport(s.http.proxy.Tr)
	s.socks = newSOCKSProxy(s.filter, recorder, s.http.destinations.DialContext)
	return s, nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.Recording.Enabled && cfg.Recording.StreamDir == "" {
		if cfg.DatabaseDSN != "" {
			cfg.Recording.StreamDir = filepath.Join(filepath.Dir(cfg.DatabaseDSN), "proxy-streams")
		} else {
			cfg.Recording.StreamDir = DefaultConfig().Recording.StreamDir
		}
	}
	if cfg.Recording.Enabled && cfg.Recording.BodyDir == "" {
		if cfg.DatabaseDSN != "" {
			cfg.Recording.BodyDir = filepath.Join(filepath.Dir(cfg.DatabaseDSN), "proxy-bodies")
		} else {
			cfg.Recording.BodyDir = DefaultConfig().Recording.BodyDir
		}
	}
	return cfg
}

// ApplyConfig hot-swaps runtime policy from cfg. Listener, certificate, cache,
// and database settings are startup-only and are intentionally ignored here.
// The sentinel set and swap tuning are installed in the running swapper rather
// than a new one, so a resolved value survives the apply unless its sentinel
// left its client's set.
func (s *Server) ApplyConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	flt := buildFilter(cfg)
	gateways := buildGatewayTables(cfg.Gateways)
	trusts, err := buildTrustTable(cfg.Trusts, s.http.proxy.Tr, s.http.trustTable())
	if err != nil {
		return err
	}
	s.filter = flt
	s.cfg.Allowlist = cfg.Allowlist
	s.cfg.Passthrough = cfg.Passthrough
	s.cfg.Gateways = cfg.Gateways
	s.cfg.Secrets = cfg.Secrets
	s.cfg.Trusts = cfg.Trusts
	s.http.setPolicy(flt, gateways)
	s.http.swapper.Reconfigure(swapperConfig(cfg))
	s.http.setTrusts(trusts)
	s.socks.setFilter(flt)
	return nil
}

func buildFilter(cfg Config) *filter.Filter {
	return filter.New(filter.Config{Clients: cfg.Allowlist.Clients, Passthrough: cfg.Passthrough.Clients})
}

func swapperConfig(cfg Config) secrets.Config {
	sentinels := make(map[string][]string, len(cfg.Secrets.Clients))
	raw := make(map[string][]string, len(cfg.Secrets.Clients))
	generations := make(map[string]map[string]string, len(cfg.Secrets.Clients))
	exchanges := map[string]bool{}
	for _, client := range cfg.Secrets.Clients {
		sentinels[client.ClientID] = client.Sentinels
		if client.Exchanges {
			exchanges[client.ClientID] = true
		}
		if len(client.Raw) > 0 {
			raw[client.ClientID] = client.Raw
		}
		if len(client.Generations) > 0 {
			generations[client.ClientID] = client.Generations
		}
	}
	return secrets.Config{
		Sentinels:       sentinels,
		Raw:             raw,
		Generations:     generations,
		Exchanges:       exchanges,
		ScanQuery:       cfg.Secrets.ScanQuery,
		PositiveTTL:     time.Duration(cfg.Secrets.PositiveTTLSeconds) * time.Second,
		NegativeTTL:     time.Duration(cfg.Secrets.NegativeTTLSeconds) * time.Second,
		RefreshInterval: time.Duration(cfg.Secrets.RefreshIntervalSeconds) * time.Second,
		ResolveRetry:    time.Duration(cfg.Secrets.ResolveRetrySeconds) * time.Second,
		GateHost:        cfg.Secrets.GateHost,
	}
}

// ListenAndServe binds the configured address and serves the mTLS proxy on
// it (Serve).
func (s *Server) ListenAndServe() error {
	var listenConfig net.ListenConfig
	tcp, err := listenConfig.Listen(s.ctx, "tcp", s.cfg.ListenAddress)
	if err != nil {
		return err
	}
	return s.Serve(tcp)
}

// Serve serves the mTLS proxy on tcp, a listener bound for it: by
// ListenAndServe, or a socket systemd holds for a pool, which the proxy
// taking over from this one serves too (ADR 26-11-26-536 §4). On a server
// already closed it returns nil without serving, and once a drain has closed
// the listener it returns nil.
func (s *Server) Serve(tcp net.Listener) error {
	listener := tls.NewListener(tcp, &tls.Config{
		Certificates: []tls.Certificate{s.certs.ServerCert},
		ClientCAs:    s.certs.ClientCAPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})
	s.connMu.Lock()
	select {
	case <-s.closed:
		s.connMu.Unlock()
		_ = listener.Close()
		return nil
	case <-s.draining:
		s.connMu.Unlock()
		_ = listener.Close()
		return nil
	default:
	}
	s.listener = listener
	s.startRetentionSweeper()
	s.connMu.Unlock()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			case <-s.draining:
				return nil
			default:
				return err
			}
		}
		if !s.admit(conn) {
			_ = conn.Close()
			return nil
		}
		go s.serveConn(conn)
	}
}

// admit tracks an accepted connection for Close to close and wait on, unless
// the server has closed.
func (s *Server) admit(conn net.Conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	select {
	case <-s.closed:
		return false
	default:
	}
	s.conns[conn] = struct{}{}
	s.wg.Add(1)
	return true
}

// startRetentionSweeper runs the audit retention pass on an interval derived
// from the configured window, beginning with one immediate pass so a proxy that
// was down longer than the window reclaims on start rather than an interval
// later. The spool budget pass runs beside it on the same tick, and also
// whenever spool writes take the trees over their budget (ADR 26-10-08-698).
// Both run on this one goroutine, so they never race over the same file.
//
// It is driven from the configuration rather than by the pool agent so that the
// bound travels with the recorder: anything that records is swept, and there is
// no deployment in which the two can be wired apart.
//
// The caller holds connMu, so Close either waits for the sweeper or stops it
// from starting.
//
// With a SweepGate, its first pass waits on it, and so does dropping the
// columns this schema retired, which it does first: a proxy still draining
// beside this one writes the same database and trees (ADR 26-11-26-536 §3).
func (s *Server) startRetentionSweeper() {
	retention := s.cfg.Recording.Retention
	budgeted := s.audit.SpoolBudget().Enabled()
	if !s.cfg.Recording.Enabled {
		return
	}
	sweeps := retention > 0 || budgeted
	interval := maxSweepInterval
	if retention > 0 {
		interval = SweepInterval(retention)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if gate := s.cfg.Recording.SweepGate; gate != nil {
			gated, stop := context.WithCancel(s.ctx)
			defer stop()
			go func() {
				select {
				case <-s.closed:
				case <-s.sweepStop:
				case <-gated.Done():
				}
				stop()
			}()
			if gate(gated) != nil {
				return
			}
			s.retireColumns()
		}
		if !sweeps {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastBudgetPass time.Time
		for {
			if retention > 0 {
				s.sweepAudit(retention)
			}
			if budgeted {
				s.enforceSpoolBudget()
				lastBudgetPass = time.Now()
			}
			select {
			case <-s.closed:
				return
			case <-s.sweepStop:
				return
			case <-s.ctx.Done():
				return
			case <-s.audit.OverBudget():
				// A long upload keeps the total over the budget while it is
				// written, and its tail cannot be cut until it closes, so
				// every write would wake a pass that walks both trees. Passes
				// woken this way are spaced out instead.
				if wait := budgetPassSpacing - time.Since(lastBudgetPass); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-s.closed:
						timer.Stop()
						return
					case <-s.sweepStop:
						timer.Stop()
						return
					case <-s.ctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
				s.enforceSpoolBudget()
				lastBudgetPass = time.Now()
				continue
			case <-ticker.C:
			}
		}
	}()
}

// handshakeTimeout bounds a client's mTLS handshake with the proxy.
const handshakeTimeout = 10 * time.Second

// budgetPassSpacing is the least time between budget passes woken by spool
// writes. It bounds how far past the budget a burst of writes can take the
// trees before a pass reclaims them.
const budgetPassSpacing = 10 * time.Second

// sweepAudit runs one retention pass and reports it on a span. The proxy has no
// logger of its own, and this is where the rest of its background work — cache
// stores, audit writes — already reports.
func (s *Server) sweepAudit(retention time.Duration) {
	ctx, span := proxyTracer().Start(contextOrBackground(s.ctx), "proxy.audit.retention.sweep")
	defer span.End()
	result, err := s.audit.Sweep(ctx, time.Now().UTC().Add(-retention))
	span.SetAttributes(
		attribute.Int64("proxy.audit.retention.http_rows", result.HTTPRows),
		attribute.Int64("proxy.audit.retention.socks_rows", result.SOCKSRows),
		attribute.Int64("proxy.audit.retention.dns_rows", result.DNSRows),
		attribute.Int64("proxy.audit.retention.files", result.Files),
		attribute.Int64("proxy.audit.retention.bytes", result.Bytes),
	)
	recordSpanError(span, err)
}

// retireColumns drops the audit columns this schema retired, and reports it
// on a span.
func (s *Server) retireColumns() {
	ctx, span := proxyTracer().Start(contextOrBackground(s.ctx), "proxy.audit.retire_columns")
	defer span.End()
	recordSpanError(span, s.audit.DropRetiredColumns(ctx))
}

// enforceSpoolBudget runs one spool budget pass and reports it on a span, as
// sweepAudit does.
func (s *Server) enforceSpoolBudget() {
	ctx, span := proxyTracer().Start(contextOrBackground(s.ctx), "proxy.audit.budget.enforce")
	defer span.End()
	result, err := s.audit.EnforceBudget(ctx)
	span.SetAttributes(
		attribute.Int64("proxy.audit.budget.limit", result.Limit),
		attribute.Int64("proxy.audit.budget.bytes", result.Bytes),
		attribute.Int64("proxy.audit.budget.remaining", result.Remaining),
		attribute.Int64("proxy.audit.budget.truncated", result.Truncated),
		attribute.Int64("proxy.audit.budget.deleted", result.Deleted),
	)
	recordSpanError(span, err)
}

// DNSAuditEvent is one DNS query to record in the proxy's audit trail.
type DNSAuditEvent = audit.DNSEvent

// RecordDNS audits a DNS query answered in this process on the proxy's behalf:
// the pool's sandbox DNS runs beside the proxy and shares its trail, its
// client identity and its retention (ADR 0148). It never blocks; under burst
// the event is dropped and counted in /audit/dropped like any other.
func (s *Server) RecordDNS(event DNSAuditEvent) {
	s.audit.RecordDNS(event)
}

// RawSecret is one value a client holds of a raw binding (ADR 26-10-27-840),
// as it is handed over: the binding's sentinel, which names it, the value,
// and when it lapses, zero when it does not.
type RawSecret = secrets.RawValue

// RawSecrets is every raw value clientID holds now, each asked for again once
// its refresh is due; one the resolver denies is left out. It is what the
// sandbox is handed, from the values this proxy already watches its requests
// for, and is never written anywhere.
func (s *Server) RawSecrets(ctx context.Context, clientID string) []RawSecret {
	if s == nil || s.http == nil {
		return nil
	}
	return s.http.swapper.RawValues(ctx, clientID)
}

// ClientHTTPAudit reads one client's audited HTTP exchanges in this process,
// for a caller that has already established who the client is: the pool's
// credentials endpoint, from the certificate a sandbox reached it over
// (ADR 26-10-02-678). It never reads the pool-wide trail, so an empty client
// is an error rather than every client's rows; that read is the control API's,
// behind its token.
func (s *Server) ClientHTTPAudit(ctx context.Context, clientID string, query AuditQuery) ([]AuditHTTPExchange, error) {
	if clientID == "" {
		return nil, errors.New("an audit read for a client names the client")
	}
	query.ClientID = clientID
	return s.audit.ListHTTP(ctx, query)
}

// StopAccepting closes the proxy's listener, and nothing else: the
// connections it accepted are still served, and Serve returns nil. Under a
// handover the listener is a socket systemd holds, which the proxy taking
// over accepts from (ADR 26-11-26-536 §4).
func (s *Server) StopAccepting() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	select {
	case <-s.draining:
	default:
		close(s.draining)
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
}

// Drain stops accepting, and serves the connections the proxy holds to their
// end, until ctx ends, which is when it returns ctx's error (ADR 26-11-26-536
// §3): a plain HTTP connection closes once its current response is sent,
// while a CONNECT tunnel, a MITM'd one included, and an upgraded stream run
// until they end. The retention sweeper stops at once: the proxy taking over
// sweeps the same trees. The caller closes the proxy afterwards (Close),
// which cuts what is left and flushes the audit.
func (s *Server) Drain(ctx context.Context) error {
	s.StopAccepting()
	s.sweepOnce.Do(func() { close(s.sweepStop) })
	if s.http != nil {
		s.http.drain()
	}
	served := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(served)
	}()
	select {
	case <-served:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the proxy and flushes queued audit events.
func (s *Server) Close() error {
	s.connMu.Lock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.connMu.Unlock()
	// Every connection's serving goroutine, then every exchange still to be
	// recorded when its body stream finishes, before the recorder closes;
	// the two share one 30s bound.
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if s.http != nil {
			select {
			case <-s.http.recording.wait():
			case <-timeout.C:
			}
		}
	case <-timeout.C:
	}
	if s.http != nil {
		s.http.reports.close()
	}
	return s.audit.Close()
}

// Addr returns the listener address after ListenAndServe starts.
func (s *Server) Addr() net.Addr {
	if s == nil {
		return nil
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) serveConn(conn net.Conn) {
	ctx, span := proxyTracer().Start(s.ctx, "proxy.connection", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	defer s.wg.Done()
	defer func() {
		s.connMu.Lock()
		_, stillTracked := s.conns[conn]
		if stillTracked {
			delete(s.conns, conn)
		}
		s.connMu.Unlock()
		if stillTracked {
			_ = conn.Close()
		}
	}()
	if tlsConn, ok := conn.(*tls.Conn); ok {
		// Bounded, since a connection holds descriptors here before any
		// certificate names it: through a pool's tunnel listener that is
		// anyone who reaches the edge (ADR 26-10-23-719).
		_ = tlsConn.SetDeadline(time.Now().Add(handshakeTimeout))
		if err := tlsConn.HandshakeContext(s.ctx); err != nil {
			recordSpanError(span, err)
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
	}
	client := clientIdentityFromConn(conn)
	span.SetAttributes(clientAttrs(client)...)
	proto, peeked, err := detect(conn)
	if err != nil {
		recordSpanError(span, err)
		return
	}
	span.SetAttributes(attribute.String("proxy.protocol", proto.String()))
	switch proto {
	case protocolHTTP:
		s.http.serveConn(ctx, peeked, client)
	case protocolSOCKS5:
		if err := s.socks.serveConn(ctx, peeked, client); err != nil {
			recordSpanError(span, err)
		}
	}
}
