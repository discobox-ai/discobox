package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// failingTreeRuntime exports a tree whose walk fails after the response has
// begun.
type failingTreeRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
}

func (failingTreeRuntime) ExportTree(context.Context, string) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write(make([]byte, 1024))
		_ = writer.CloseWithError(errors.New("walk failed"))
	}()
	return reader, nil
}

// A walk that fails once the body has begun has to reach the caller as a
// failed read. Returning from the handler instead lets net/http end the chunked
// body cleanly, and the caller holds a short tar that ends like a whole one.
func TestExportTreeAbortsTheResponseWhenTheWalkFails(t *testing.T) {
	const projectID, poolID, sandboxID = "project-1", "pool-1", "sandbox-1"
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               failingTreeRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime()},
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		server.URL+"/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/tree", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeSandboxRead))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the failure comes after the body has begun", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("the body ended cleanly; a failed walk has to read as a failure")
	}
}
