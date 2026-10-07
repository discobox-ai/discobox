package sandboxruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// emptyDaemon is a Docker daemon holding no sandbox containers at all.
func emptyDaemon(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli, projectID: "proj_a", poolID: "pool_a"}
}

// An explicit power instruction for a sandbox with no container answers why
// there is none, the way the on-demand path does: a tree here with no
// container is a sandbox to repair, not a missing one, and an archived tree is
// archived. Only an id whose tree is not here is not found. Each answers at
// once — the rebuild wait is the attach route's, not an instruction's.
func TestPowerInstructionsTellWhyASandboxHasNoContainer(t *testing.T) {
	withTestRoot(t)
	runtime := emptyDaemon(t)
	if err := os.MkdirAll(runtime.sandboxRoot("sbx_lost"), 0o755); err != nil {
		t.Fatalf("create sandbox tree: %v", err)
	}
	archivedRoot := runtime.sandboxRoot("sbx_archived")
	if err := os.MkdirAll(archivedRoot, 0o755); err != nil {
		t.Fatalf("create sandbox tree: %v", err)
	}
	if err := os.WriteFile(sandboxArchiveMarkerPath(archivedRoot), nil, 0o644); err != nil {
		t.Fatalf("mark sandbox archived: %v", err)
	}

	operations := map[string]func(context.Context, string) error{
		"start":   func(ctx context.Context, id string) error { return runtime.StartSandbox(ctx, id, nil) },
		"stop":    func(ctx context.Context, id string) error { return runtime.StopSandbox(ctx, id, nil) },
		"restart": func(ctx context.Context, id string) error { return runtime.RestartSandbox(ctx, id, nil) },
	}
	cases := []struct {
		sandboxID string
		want      error
	}{
		{"sbx_lost", ErrNoContainer},
		{"sbx_archived", ErrArchived},
		{"sbx_elsewhere", ErrNotFound},
	}
	for name, operate := range operations {
		for _, tc := range cases {
			t.Run(name+"/"+tc.sandboxID, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				started := time.Now()
				err := operate(ctx, tc.sandboxID)
				if !errors.Is(err, tc.want) {
					t.Fatalf("%s %s = %v, want %v", name, tc.sandboxID, err, tc.want)
				}
				if elapsed := time.Since(started); elapsed > 2*time.Second {
					t.Fatalf("%s %s answered after %s, want at once", name, tc.sandboxID, elapsed)
				}
			})
		}
	}
}
