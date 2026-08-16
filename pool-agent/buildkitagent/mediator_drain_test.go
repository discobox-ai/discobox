package buildkitagent_test

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/obot-platform/discobox/pool-agent/buildkitagent"
)

// servingMediator starts a gRPC server whose one handler blocks until release
// is closed, standing in for a build stream still running when a stop arrives.
// It returns the server and a channel reporting that the handler returned.
func servingMediator(t *testing.T, release <-chan struct{}) (*buildkitagent.Mediator, *grpc.Server, <-chan struct{}) {
	t.Helper()
	handled := make(chan struct{})
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		defer close(handled)
		select {
		case <-release:
		case <-stream.Context().Done():
		}
		return nil
	}))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { srv.Stop() })

	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial mediator: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// One stream open against the handler, which is what a build in flight is.
	if _, err := conn.NewStream(context.Background(), &grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, "/build/Solve"); err != nil {
		t.Fatalf("open stream: %v", err)
	}

	return buildkitagent.NewTestMediator(slog.New(slog.DiscardHandler)), srv, handled
}

// A stop waits for the builds already running rather than cutting them off.
func TestAStopCarriesTheBuildsAlreadyRunning(t *testing.T) {
	release := make(chan struct{})
	mediator, srv, handled := servingMediator(t, release)

	stopped := make(chan struct{})
	go func() {
		mediator.Drain(srv, time.Minute)
		close(stopped)
	}()

	// The drain must still be waiting: the build has not finished.
	select {
	case <-stopped:
		t.Fatal("the stop did not wait for the build still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-handled:
	case <-time.After(10 * time.Second):
		t.Fatal("the build never finished")
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop did not return once the build finished")
	}
}

// A build that never finishes must not hold a restart open forever.
func TestABuildThatNeverFinishesIsBounded(t *testing.T) {
	release := make(chan struct{}) // never closed
	mediator, srv, _ := servingMediator(t, release)

	start := time.Now()
	mediator.Drain(srv, 200*time.Millisecond)

	if waited := time.Since(start); waited > 10*time.Second {
		t.Fatalf("the stop waited %s, want it bounded by the drain", waited)
	}
}
