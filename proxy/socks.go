package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/discobox-ai/discobox/proxy/internal/filter"
	"github.com/things-go/go-socks5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type socksProxy struct {
	server *socks5.Server
	filter *filter.Filter
	mu     sync.RWMutex
	ids    map[string]clientIdentity
}

func newSOCKSProxy(flt *filter.Filter, recorder *audit.Recorder) *socksProxy {
	s := &socksProxy{filter: flt, ids: map[string]clientIdentity{}}
	s.server = socks5.NewServer(
		socks5.WithRule(&socksRule{audit: recorder, identities: s}),
		socks5.WithAuthMethods([]socks5.Authenticator{socks5.NoAuthAuthenticator{}}),
		socks5.WithLogger(socksLogger{}),
	)
	return s
}

func (s *socksProxy) setFilter(flt *filter.Filter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = flt
}

func (s *socksProxy) currentFilter() *filter.Filter {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.filter
}

func (s *socksProxy) serveConn(ctx context.Context, conn net.Conn, identity clientIdentity) error {
	_, span := proxyTracer().Start(ctx, "proxy.socks.connection", trace.WithAttributes(clientAttrs(identity)...))
	defer span.End()
	key := ""
	if conn.RemoteAddr() != nil {
		key = conn.RemoteAddr().String()
		s.mu.Lock()
		s.ids[key] = identity
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.ids, key)
			s.mu.Unlock()
		}()
	}
	err := s.server.ServeConn(conn)
	recordSpanError(span, err)
	return err
}

func (s *socksProxy) identity(remote net.Addr) clientIdentity {
	if remote == nil {
		return clientIdentity{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ids[remote.String()]
}

type socksRule struct {
	audit      *audit.Recorder
	identities *socksProxy
}

func (r *socksRule) Allow(ctx context.Context, req *socks5.Request) (context.Context, bool) {
	host := req.DestAddr.FQDN
	if host == "" {
		host = req.DestAddr.IP.String()
	}
	client := r.identities.identity(req.RemoteAddr)
	allowed := r.identities.currentFilter().AllowHostForClient(host, client.ID)
	ctx, span := proxyTracer().Start(ctx, "proxy.socks.connect",
		trace.WithAttributes(append(clientAttrs(client),
			attribute.String("server.address", host),
			attribute.Int("server.port", req.DestAddr.Port),
			attribute.Bool("proxy.allowed", allowed),
		)...),
	)
	defer span.End()
	r.audit.RecordSOCKS(audit.SOCKSEvent{
		Context:       ctx,
		Time:          time.Now().UTC(),
		ClientID:      client.ID,
		ClientSubject: client.Subject,
		ClientSerial:  client.Serial,
		Destination:   host,
		Port:          req.DestAddr.Port,
		Allowed:       allowed,
		BlockedReason: blockedReason(allowed),
	})
	return ctx, allowed
}

func blockedReason(allowed bool) string {
	if allowed {
		return ""
	}
	return "host denied"
}

type socksLogger struct{}

func (socksLogger) Errorf(format string, args ...any) {
	_ = fmt.Sprintf(format, args...)
}
