package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	grpcSentinel  = "sentinel-grpc-token"
	grpcRealValue = "real-grpc-token"
)

// grpcOrigin echoes what it is sent, prefixed with the credential it was sent
// with, so a test sees both that the call worked and that the proxy swapped.
type grpcOrigin struct {
	testgrpc.UnimplementedTestServiceServer
}

func grpcCredential(ctx context.Context) string {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) > 0 {
		return values[0]
	}
	return ""
}

func (grpcOrigin) UnaryCall(ctx context.Context, req *testgrpc.SimpleRequest) (*testgrpc.SimpleResponse, error) {
	body := string(req.GetPayload().GetBody())
	if body == "missing" {
		// A status is carried in the trailers, so this is what proves they
		// survived both legs.
		return nil, status.Error(codes.NotFound, "no such thing")
	}
	return &testgrpc.SimpleResponse{Payload: &testgrpc.Payload{Body: []byte(grpcCredential(ctx) + " " + body)}}, nil
}

func (grpcOrigin) FullDuplexCall(stream testgrpc.TestService_FullDuplexCallServer) error {
	credential := grpcCredential(stream.Context())
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		reply := &testgrpc.StreamingOutputCallResponse{Payload: &testgrpc.Payload{Body: []byte(credential + " " + string(req.GetPayload().GetBody()))}}
		if err := stream.Send(reply); err != nil {
			return err
		}
	}
}

// startGRPCOrigin serves grpcOrigin, over TLS with a self-signed certificate
// when secure, and as cleartext HTTP/2 when not.
func startGRPCOrigin(t *testing.T, secure bool) net.Addr {
	t.Helper()
	var opts []grpc.ServerOption
	if secure {
		opts = append(opts, grpc.Creds(credentials.NewServerTLSFromCert(selfSignedCertificate(t))))
	}
	server := grpc.NewServer(opts...)
	testgrpc.RegisterTestServiceServer(server, grpcOrigin{})
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr()
}

func selfSignedCertificate(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "grpc origin"},
		DNSNames:     []string{"grpc.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startGRPCProxy runs a proxy that swaps grpcSentinel for grpcRealValue and sends
// everything it is asked to reach to origin.
func startGRPCProxy(ctx context.Context, t *testing.T, origin net.Addr) (net.Addr, ClientMaterial) {
	t.Helper()
	dir := t.TempDir()
	prepared, err := PrepareCertificates(PrepareOptions{
		Dir:         filepath.Join(dir, "certs"),
		ServerHosts: []string{"127.0.0.1", "localhost"},
		ClientIDs:   []string{"sandbox-1"},
	})
	if err != nil {
		t.Fatalf("PrepareCertificates() error = %v", err)
	}
	server, err := NewServer(ctx, Config{
		ListenAddress: "127.0.0.1:0",
		CertDir:       prepared.Bundle.Dir,
		DatabaseDSN:   filepath.Join(dir, "audit.db"),
		Recording:     RecordingConfig{Enabled: true, QueueSize: 16},
		Secrets:       SecretsConfig{Clients: []SecretClient{{ClientID: "sandbox-1", Sentinels: []string{grpcSentinel}}}},
	}, prepared.Bundle, stubResolver{value: grpcRealValue, useID: "use_grpc"})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	// The proxy's own transport, sent straight to the origin (not through
	// whatever proxy this test's environment names) and trusting its
	// self-signed certificate, but otherwise as built, so the h2 it
	// negotiates is the proxy's.
	var dialer net.Dialer
	transport := server.http.proxy.Tr
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, origin.String())
	}
	// Set on the configuration the transport was built with, not a new one:
	// h2 is already configured into that one's ALPN.
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test origin is self-signed
	server.http.h2c = cleartextHTTP2Transport(transport)
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	t.Cleanup(closeProxyServer(t, server, errCh))
	return waitForAddr(t, server), prepared.Clients["sandbox-1"]
}

// dialGRPCThroughProxy connects a gRPC client to authority through a CONNECT
// tunnel, the way a gRPC client honoring HTTPS_PROXY does. Over TLS it trusts
// only the MITM CA, so a call that works is a call the proxy intercepted.
func dialGRPCThroughProxy(ctx context.Context, t *testing.T, addr net.Addr, material ClientMaterial, authority string, secure bool) testgrpc.TestServiceClient {
	t.Helper()
	tunnel := openTunnel(ctx, t, addr, material, authority)
	_ = tunnel.SetDeadline(time.Time{})
	tunnels := make(chan net.Conn, 1)
	tunnels <- tunnel
	creds := insecure.NewCredentials()
	if secure {
		pem, err := os.ReadFile(material.MITMCAPath)
		if err != nil {
			t.Fatalf("read MITM CA: %v", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			t.Fatal("parse MITM CA")
		}
		creds = credentials.NewTLS(&tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient("passthrough:///"+authority,
		grpc.WithTransportCredentials(creds),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			select {
			case tunnel := <-tunnels:
				return tunnel, nil
			default:
				return nil, errors.New("the test opened one tunnel")
			}
		}),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return testgrpc.NewTestServiceClient(conn)
}

// gRPC is HTTP/2 end to end and the proxy still intercepts it: the sandbox's
// leg is h2 under the MITM certificate, the origin's is h2 to the origin, and
// a sentinel in the call's metadata reaches the origin swapped. Unary results,
// statuses carried in trailers, and a bidirectional stream whose client waits
// for each reply before sending the next all make it through.
func TestGRPCThroughMITM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("goproxy's MITM leg fails before the handler runs on Windows")
	}
	for _, tc := range []struct {
		name      string
		secure    bool
		authority string
	}{
		{name: "TLS", secure: true, authority: "grpc.example.test:443"},
		{name: "h2c", secure: false, authority: "grpc.example.test:50051"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			origin := startGRPCOrigin(t, tc.secure)
			addr, material := startGRPCProxy(ctx, t, origin)
			client := dialGRPCThroughProxy(ctx, t, addr, material, tc.authority, tc.secure)
			callCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+grpcSentinel)

			resp, err := client.UnaryCall(callCtx, &testgrpc.SimpleRequest{Payload: &testgrpc.Payload{Body: []byte("hello")}})
			if err != nil {
				t.Fatalf("UnaryCall() error = %v", err)
			}
			if got, want := string(resp.GetPayload().GetBody()), "Bearer "+grpcRealValue+" hello"; got != want {
				t.Fatalf("UnaryCall() = %q, want %q", got, want)
			}

			_, err = client.UnaryCall(callCtx, &testgrpc.SimpleRequest{Payload: &testgrpc.Payload{Body: []byte("missing")}})
			if got := status.Code(err); got != codes.NotFound || status.Convert(err).Message() != "no such thing" {
				t.Fatalf("UnaryCall(missing) error = %v, want NotFound \"no such thing\"", err)
			}

			stream, err := client.FullDuplexCall(callCtx)
			if err != nil {
				t.Fatalf("FullDuplexCall() error = %v", err)
			}
			for _, message := range []string{"one", "two", "three"} {
				if err := stream.Send(&testgrpc.StreamingOutputCallRequest{Payload: &testgrpc.Payload{Body: []byte(message)}}); err != nil {
					t.Fatalf("Send(%s) error = %v", message, err)
				}
				reply, err := stream.Recv()
				if err != nil {
					t.Fatalf("Recv() after %s error = %v", message, err)
				}
				if got, want := string(reply.GetPayload().GetBody()), "Bearer "+grpcRealValue+" "+message; got != want {
					t.Fatalf("reply to %s = %q, want %q", message, got, want)
				}
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatalf("CloseSend() error = %v", err)
			}
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("Recv() after CloseSend error = %v, want EOF", err)
			}
		})
	}
}
