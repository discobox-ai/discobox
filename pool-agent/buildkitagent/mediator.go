package buildkitagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	controlapi "github.com/moby/buildkit/api/services/control"
	gatewayapi "github.com/moby/buildkit/frontend/gateway/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/proto"
)

// The two methods a build's intent arrives on. Both are rewritten: buildx
// drives the dockerfile frontend through the gateway, so its build-args and
// policies land on LLBBridge/Solve, while a buildctl client takes the direct
// path. Handling only one silently misses half the clients — and fails open,
// since an un-rewritten solve is an unconstrained one.
const (
	solveMethod        = "/moby.buildkit.v1.Control/Solve"
	gatewaySolveMethod = "/moby.buildkit.v1.frontend.LLBBridge/Solve"
)

// insecureEntitlements are refused on every build. A host-network step runs in
// buildkitd's own namespace, where the per-build loopback that binds a build to
// its sandbox does not exist and every other build's forwarder is reachable;
// security.insecure drops isolation outright. BuildKit denies both by default,
// so this is defense in depth against a daemon started with them allowed.
var insecureEntitlements = map[string]struct{}{
	"network.host":      {},
	"security.insecure": {},
}

// Mediator is the only way a sandbox reaches the pool's buildkitd. It
// terminates mTLS, so every build carries the identity of the sandbox whose
// client certificate opened the connection, and rewrites the solve requests
// that identity governs before forwarding them over buildkitd's Unix socket.
//
// Every other method is forwarded as opaque bytes. That is what keeps the
// session — filesync for the build context, registry auth, secrets and ssh —
// working without this package modeling any of it.
type Mediator struct {
	upstream *grpc.ClientConn
	logger   *slog.Logger
}

// rawMessage is the payload the passthrough codec moves.
type rawMessage struct{ data []byte }

// rawCodec registers as "proto" so gRPC hands every message through untouched.
// It is what lets one handler forward methods this package has no types for.
type rawCodec struct{}

func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*rawMessage)
	if !ok {
		return nil, fmt.Errorf("buildkit mediator: unexpected marshal type %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.data)}, nil
}

func (rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*rawMessage)
	if !ok {
		return fmt.Errorf("buildkit mediator: unexpected unmarshal type %T", v)
	}
	m.data = data.Materialize()
	return nil
}

func (rawCodec) Name() string { return "proto" }

// NewMediator dials buildkitd and returns a mediator ready to serve.
func NewMediator(logger *slog.Logger) (*Mediator, error) {
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := grpc.NewClient("unix://"+Socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodecV2(rawCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("dial buildkitd: %w", err)
	}
	return &Mediator{upstream: conn, logger: logger}, nil
}

// Close releases the upstream connection.
func (m *Mediator) Close() error { return m.upstream.Close() }

// Serve accepts mTLS connections until ctx is canceled.
func (m *Mediator) Serve(ctx context.Context, listener net.Listener, tlsConfig *tls.Config) error {
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.ForceServerCodecV2(rawCodec{}),
		grpc.UnknownServiceHandler(m.handle),
	)
	go func() {
		<-ctx.Done()
		srv.Stop()
	}()
	return srv.Serve(listener)
}

// sandboxID returns the identity of the peer whose certificate opened this
// stream. The client certificate is the sandbox's existing proxy credential,
// whose common name is its sandbox ID — already the proxy's tenant boundary,
// so a build and its egress are attributed to the same subject.
func sandboxID(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", errors.New("no peer on stream")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return "", errors.New("peer is not mTLS-authenticated")
	}
	chains := tlsInfo.State.VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return "", errors.New("peer presented no verified certificate")
	}
	name := chains[0][0].Subject.CommonName
	if name == "" {
		return "", errors.New("peer certificate carries no common name")
	}
	return name, nil
}

func (m *Mediator) handle(_ any, ss grpc.ServerStream) error {
	err := m.forward(ss)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
		// Without this a mediator failure is invisible: the client sees only a
		// stalled connection, and nothing else here logs per stream.
		method, _ := grpc.MethodFromServerStream(ss)
		m.logger.Warn("buildkit stream failed", "method", method, "error", err)
	}
	return err
}

func (m *Mediator) forward(ss grpc.ServerStream) error {
	method, ok := grpc.MethodFromServerStream(ss)
	if !ok {
		return errors.New("no method on stream")
	}
	m.logger.Debug("buildkit stream", "method", method)
	ctx := ss.Context()
	// Refuse anything we cannot attribute. A build whose owner is unknown is a
	// build no policy can be applied to.
	id, err := sandboxID(ctx)
	if err != nil {
		return fmt.Errorf("reject build: %w", err)
	}

	forwardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		forwardCtx = metadata.NewOutgoingContext(forwardCtx, md.Copy())
	}

	cs, err := m.upstream.NewStream(forwardCtx,
		&grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method)
	if err != nil {
		return err
	}

	errc := make(chan error, 2)
	var once sync.Once
	sendHeader := func() {
		once.Do(func() {
			if md, err := cs.Header(); err == nil && md != nil {
				_ = ss.SendHeader(md)
			}
		})
	}

	go func() { errc <- m.pumpToUpstream(ss, cs, method, id) }()
	go func() {
		for {
			var msg rawMessage
			if err := cs.RecvMsg(&msg); err != nil {
				sendHeader()
				errc <- err
				return
			}
			sendHeader()
			if err := ss.SendMsg(&msg); err != nil {
				errc <- err
				return
			}
		}
	}()

	for range 2 {
		if err := <-errc; err != nil && !errors.Is(err, io.EOF) {
			ss.SetTrailer(cs.Trailer())
			return err
		}
	}
	ss.SetTrailer(cs.Trailer())
	return nil
}

// pumpToUpstream copies client messages upstream, rewriting the solve methods.
func (m *Mediator) pumpToUpstream(ss grpc.ServerStream, cs grpc.ClientStream, method, id string) error {
	for {
		var msg rawMessage
		if err := ss.RecvMsg(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return cs.CloseSend()
			}
			return err
		}
		switch method {
		case solveMethod:
			rewritten, err := m.rewriteSolve(msg.data, id)
			if err != nil {
				return err
			}
			msg.data = rewritten
		case gatewaySolveMethod:
			rewritten, err := m.rewriteGatewaySolve(msg.data, id)
			if err != nil {
				return err
			}
			msg.data = rewritten
		}
		if err := cs.SendMsg(&msg); err != nil {
			return err
		}
	}
}

// rewriteSolve constrains a control-plane solve.
//
// SolveRequest.Ref is the build's history ID, so logging it against the sandbox
// is what makes a build's sources attributable after the fact: the gateway
// solve carries no LLB definition for a dockerfile build, and the mediator
// cannot see source identifiers itself.
func (m *Mediator) rewriteSolve(data []byte, id string) ([]byte, error) {
	var req controlapi.SolveRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("decode solve from %s: %w", id, err)
	}
	req.Entitlements = stripInsecure(req.Entitlements)
	m.logger.Info("build accepted", "sandbox", id, "ref", req.Ref, "frontend", req.Frontend)
	out, err := proto.Marshal(&req)
	if err != nil {
		return nil, fmt.Errorf("encode solve for %s: %w", id, err)
	}
	return out, nil
}

// rewriteGatewaySolve constrains the gateway solve, which is where buildx puts
// a dockerfile build's options.
func (m *Mediator) rewriteGatewaySolve(data []byte, id string) ([]byte, error) {
	var req gatewayapi.SolveRequest
	if err := proto.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("decode gateway solve from %s: %w", id, err)
	}
	// buildx drives the dockerfile frontend through the gateway, so this is
	// where a build's options actually arrive. The equivalent fields on
	// Control/Solve are empty for a buildx client.
	req.FrontendOpt = withBuildProxy(req.FrontendOpt, id)
	out, err := proto.Marshal(&req)
	if err != nil {
		return nil, fmt.Errorf("encode gateway solve for %s: %w", id, err)
	}
	return out, nil
}

// withBuildProxy points every RUN step at the forwarder bound into its own
// network namespace, so build egress leaves through the pool proxy carrying
// the owning sandbox's identity.
//
// The values are set, never merged: buildx forwards the client's own proxy
// environment as build-args, and a sandbox's HTTP_PROXY names its own loopback
// — which inside a pool-side build container is that container's loopback,
// where nothing listens. Honoring it would hang every build.
//
// The dockerfile frontend treats these names specially: they reach every RUN
// without an ARG declaration, are excluded from the cache key (so a per-sandbox
// address does not fragment the shared cache), and never land in the image
// config or history.
func withBuildProxy(opts map[string]string, sandboxID string) map[string]string {
	if opts == nil {
		opts = map[string]string{}
	}
	proxyURL := BuildProxyURL(sandboxID)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		opts["build-arg:"+name] = proxyURL
	}
	// Loopback must stay direct: the forwarder itself listens there, and a
	// build step that proxied its own loopback would loop back into it.
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		opts["build-arg:"+name] = "127.0.0.1,localhost,::1"
	}
	return opts
}

// stripInsecure removes entitlements a sandbox may not request, preserving the
// order of the rest so a request is otherwise untouched.
func stripInsecure(requested []string) []string {
	if len(requested) == 0 {
		return requested
	}
	kept := make([]string, 0, len(requested))
	for _, e := range requested {
		if _, refused := insecureEntitlements[e]; refused {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// ClientTLSConfig builds the mediator's server-side TLS configuration from the
// pool's proxy certificate bundle. Client certificates are required, not
// optional: an unauthenticated connection is a build with no owner.
func ClientTLSConfig(serverCert, serverKey, mtlsCA string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(resolve(serverCert), resolve(serverKey))
	if err != nil {
		return nil, fmt.Errorf("load mediator server certificate: %w", err)
	}
	caPEM, err := readFile(resolve(mtlsCA))
	if err != nil {
		return nil, fmt.Errorf("read mTLS CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("parse mTLS CA")
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
