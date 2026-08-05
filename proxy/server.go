package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/obot-platform/discobox/proxy/internal/audit"
	"github.com/obot-platform/discobox/proxy/internal/cache"
	"github.com/obot-platform/discobox/proxy/internal/filter"
	"github.com/obot-platform/discobox/proxy/internal/rules"
	"github.com/obot-platform/discobox/proxy/internal/secrets"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Server is the mTLS worker proxy server.
type Server struct {
	ctx         context.Context
	cfg         Config
	certs       *CertificateBundle
	audit       *audit.Recorder
	cache       *cache.Cache
	filter      *filter.Filter
	rewriter    *rules.Rewriter
	resolver    secrets.Resolver
	swapper     *secrets.Swapper
	http        *httpProxy
	socks       *socksProxy
	controlAuth *controlAuthenticator

	listener net.Listener
	connMu   sync.Mutex
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
	closed   chan struct{}
}

// NewServer creates a worker-scoped proxy server. A nil resolver disables
// sentinel secret swapping; the resolver is a stable dependency preserved across
// ApplyConfig reloads.
func NewServer(ctx context.Context, cfg Config, certs *CertificateBundle, resolver secrets.Resolver) (*Server, error) {
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
	recorder.ConfigureStreamSpool(cfg.Recording.StreamDir, cfg.Recording.StreamQueueSize)
	recorder.ConfigureBodySpool(cfg.Recording.BodyDir)
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
	flt, rewriter := buildPolicy(cfg)
	swapper := buildSwapper(cfg, resolver)
	s := &Server{
		ctx:         ctx,
		cfg:         cfg,
		certs:       certs,
		audit:       recorder,
		cache:       c,
		filter:      flt,
		rewriter:    rewriter,
		resolver:    resolver,
		swapper:     swapper,
		controlAuth: controlAuth,
		conns:       map[net.Conn]struct{}{},
		closed:      make(chan struct{}),
	}
	s.http = newHTTPProxy(certs, s.filter, s.rewriter, swapper, c, recorder)
	upstream, err := upstreamProxyURL(cfg)
	if err != nil {
		return nil, err
	}
	applyUpstreamProxy(s.http, upstream, upstreamNoProxy(cfg))
	s.socks = newSOCKSProxy(s.filter, recorder)
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
func (s *Server) ApplyConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	flt, rewriter := buildPolicy(cfg)
	swapper := buildSwapper(cfg, s.resolver)
	s.filter = flt
	s.rewriter = rewriter
	s.swapper = swapper
	s.cfg.Allowlist = cfg.Allowlist
	s.cfg.Headers = cfg.Headers
	s.cfg.Secrets = cfg.Secrets
	s.http.setPolicy(flt, rewriter)
	s.http.setSwapper(swapper)
	s.socks.setFilter(flt)
	return nil
}

func buildPolicy(cfg Config) (*filter.Filter, *rules.Rewriter) {
	return filter.New(filter.Config{
		Enabled: cfg.Allowlist.Enabled,
		Domains: cfg.Allowlist.Domains,
		IPs:     cfg.Allowlist.IPs,
		Rules:   convertAllowlistRules(cfg.Allowlist.Rules),
	}), rules.NewRewriter(convertHeaderRules(cfg.Headers))
}

func buildSwapper(cfg Config, resolver secrets.Resolver) *secrets.Swapper {
	if resolver == nil {
		return nil
	}
	sentinels := make(map[string][]string, len(cfg.Secrets.Clients))
	for _, client := range cfg.Secrets.Clients {
		sentinels[client.ClientID] = client.Sentinels
	}
	return secrets.New(resolver, secrets.Config{
		Sentinels:       sentinels,
		ScanQuery:       cfg.Secrets.ScanQuery,
		PositiveTTL:     time.Duration(cfg.Secrets.PositiveTTLSeconds) * time.Second,
		NegativeTTL:     time.Duration(cfg.Secrets.NegativeTTLSeconds) * time.Second,
		RefreshInterval: time.Duration(cfg.Secrets.RefreshIntervalSeconds) * time.Second,
	})
}

func convertAllowlistRules(rules []AllowlistRule) []filter.Rule {
	converted := make([]filter.Rule, 0, len(rules))
	for _, rule := range rules {
		converted = append(converted, filter.Rule{
			ClientIDs: rule.ClientIDs,
			Domains:   rule.Domains,
			IPs:       rule.IPs,
		})
	}
	return converted
}

func convertHeaderRules(headerRules []HeaderRule) []rules.HeaderRule {
	converted := make([]rules.HeaderRule, 0, len(headerRules))
	for _, rule := range headerRules {
		conditions := make([]rules.HeaderCondition, 0, len(rule.Conditions))
		for _, condition := range rule.Conditions {
			conditions = append(conditions, rules.HeaderCondition{Header: condition.Header, Equals: condition.Equals})
		}
		converted = append(converted, rules.HeaderRule{
			ID:          rule.ID,
			Pattern:     rule.Pattern,
			Methods:     rule.Methods,
			PathRegexes: rule.PathRegexes,
			ClientIDs:   rule.ClientIDs,
			Conditions:  conditions,
			Set:         rule.Set,
			Append:      rule.Append,
		})
	}
	return converted
}

// ListenAndServe starts the mTLS listener.
func (s *Server) ListenAndServe() error {
	var listenConfig net.ListenConfig
	tcp, err := listenConfig.Listen(s.ctx, "tcp", s.cfg.ListenAddress)
	if err != nil {
		return err
	}
	s.listener = tls.NewListener(tcp, &tls.Config{
		Certificates: []tls.Certificate{s.certs.ServerCert},
		ClientCAs:    s.certs.ClientCAPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	})
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
				return err
			}
		}
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// Close stops the proxy and flushes queued audit events.
func (s *Server) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	s.connMu.Lock()
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.connMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	}
	return s.audit.Close()
}

// Addr returns the listener address after ListenAndServe starts.
func (s *Server) Addr() net.Addr {
	if s == nil || s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *Server) serveConn(conn net.Conn) {
	ctx, span := proxyTracer().Start(s.ctx, "proxy.connection", trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()
	defer s.wg.Done()
	s.connMu.Lock()
	s.conns[conn] = struct{}{}
	s.connMu.Unlock()
	handoff := false
	defer func() {
		if handoff {
			return
		}
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
		if err := tlsConn.HandshakeContext(s.ctx); err != nil {
			recordSpanError(span, err)
			return
		}
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
		if hijacked := s.http.serveConn(ctx, peeked, client); hijacked {
			handoff = true
			return
		}
	case protocolSOCKS5:
		if err := s.socks.serveConn(ctx, peeked, client); err != nil {
			recordSpanError(span, err)
		}
	}
}
