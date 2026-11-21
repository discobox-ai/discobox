package docker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

// A pool created while the scan runs has its row committed before its
// container appears. The scan must not take that container for an orphan and
// remove it (#1473).
func TestDockerPoolWatcherKeepsPoolCreatedDuringScan(t *testing.T) {
	manager := &poolCreatedDuringScan{}
	var removed atomic.Bool
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			// The pool's row commits just before its container becomes visible.
			manager.created.Store(true)
			fmt.Fprint(w, `[{"Id":"container-1"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/container-1/json"):
			fmt.Fprintf(w, `{"Id":"container-1","Config":{"Labels":{%q:"pool-1"}}}`, dockerworker.LabelPoolID)
		case r.Method == http.MethodDelete:
			// An orphan's runtime is removed by name, with its console,
			// network and volume: any removal at all is the bug.
			removed.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(daemon.Close)
	cli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(daemon.URL, "http://")))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	engine, err := dockerworker.New(dockerworker.Config{SandboxBaseImage: "worker:test"}, &LocalDriver{client: cli, agentPort: defaultAgentPort})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	watcher := dockerPoolWatcher{client: cli, engine: engine, manager: manager, projectID: "project-1", providerID: "provider-1"}
	if _, err := watcher.scan(t.Context()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if removed.Load() {
		t.Fatal("scan removed pool-1's runtime after its pool row was committed")
	}
}

// poolCreatedDuringScan has no pools until the daemon has been asked for its
// containers: a pool created between a scan's two reads.
type poolCreatedDuringScan struct {
	recordingPoolManager
	created atomic.Bool
}

func (m *poolCreatedDuringScan) ListPoolsForProviderInstance(ctx context.Context, projectID, providerID string) ([]model.Pool, error) {
	if !m.created.Load() {
		return nil, nil
	}
	return m.recordingPoolManager.ListPoolsForProviderInstance(ctx, projectID, providerID)
}
