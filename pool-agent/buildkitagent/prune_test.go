package buildkitagent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
)

type pruneRecorder struct {
	controlapi.UnimplementedControlServer
	requests chan *controlapi.PruneRequest
}

func (p *pruneRecorder) Prune(req *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	p.requests <- req
	return stream.Send(&controlapi.UsageRecord{ID: "record-1", Size: 1024})
}

// A clear asks buildkitd for everything, not only what its own GC would drop:
// the operator wants the build cache gone, and a partial prune leaves exactly
// the records they were trying to be rid of.
func TestPruneBuildCacheAsksBuildkitdForEverything(t *testing.T) {
	// PruneBuildCache dials a Linux guest path as a unix:// target; a Windows
	// temp directory's drive letter reads as a port there and never parses.
	if runtime.GOOS == "windows" {
		t.Skip("buildkitd's socket is a guest path; the pool agent runs in the Linux guest")
	}
	root, err := os.MkdirTemp("", "bk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	old := testRoot
	testRoot = root
	t.Cleanup(func() { testRoot = old })

	socket := resolve(Socket)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	recorder := &pruneRecorder{requests: make(chan *controlapi.PruneRequest, 1)}
	controlapi.RegisterControlServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	if err := PruneBuildCache(context.Background()); err != nil {
		t.Fatalf("PruneBuildCache: %v", err)
	}
	select {
	case req := <-recorder.requests:
		if !req.All {
			t.Fatalf("prune request = %+v, want All", req)
		}
	default:
		t.Fatal("buildkitd was never asked to prune")
	}
}
