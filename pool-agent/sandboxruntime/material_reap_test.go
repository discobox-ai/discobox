package sandboxruntime

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// materialDaemon is a Docker daemon that holds sandboxID's container from the
// listing numbered appearsAt on (0 is the first), and none before it.
func materialDaemon(t *testing.T, sandboxID string, appearsAt int64) *DockerSandboxRuntime {
	t.Helper()
	var lists atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			if lists.Add(1)-1 < appearsAt {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"Id":"ctr-1"}]`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr-1/json"):
			_, _ = w.Write([]byte(`{"Id":"ctr-1","Config":{"Labels":{"` + sandboxLabelSandbox + `":"` + sandboxID + `"}},"State":{"Status":"created"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli, root: withTestRoot(t), projectID: "proj_a", poolID: "pool_a"}
}

// stageAgedMaterial is what a sandbox created long ago holds on this pool: its
// material record, made when it was first created, and its sentinels.
func stageAgedMaterial(t *testing.T, r *DockerSandboxRuntime, sandboxID string) {
	t.Helper()
	if _, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID); err != nil {
		t.Fatalf("EnsureSandboxMaterial() error = %v", err)
	}
	past := time.Now().Add(-72 * time.Hour)
	record := filepath.Join(proxyagent.PoolSandboxMaterialRoot(r.root, r.projectID, r.poolID), sandboxID)
	if err := os.Chtimes(record, past, past); err != nil {
		t.Fatalf("age material record: %v", err)
	}
	if err := proxyagent.UpsertSandboxSentinels(r.root, r.projectID, r.poolID, sandboxID, []string{"sk-" + sandboxID}, nil); err != nil {
		t.Fatalf("UpsertSandboxSentinels() error = %v", err)
	}
}

func sentinelsRegistered(t *testing.T, r *DockerSandboxRuntime, sandboxID string) bool {
	t.Helper()
	data, err := os.ReadFile(r.root.ProxySecretsFile(r.projectID, r.poolID))
	if err != nil {
		t.Fatalf("read proxy secrets: %v", err)
	}
	var doc struct {
		Clients map[string][]string `json:"clients"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode proxy secrets: %v", err)
	}
	_, ok := doc.Clients[sandboxID]
	return ok
}

// A sandbox whose container is being replaced has none between the old
// container's removal and the new one's create, after the create has already
// registered the new container's sentinels. Its material record is as old as
// the sandbox, so the grace period does not spare it. The create holds the
// sandbox's lock across that gap, and the reaper must not reap under it (#314).
func TestMaterialReaperSparesASandboxWhoseCreateHoldsItsLock(t *testing.T) {
	r := materialDaemon(t, "sbx_1", 1<<62)
	stageAgedMaterial(t, r, "sbx_1")
	logger := slog.New(slog.DiscardHandler)

	lock := r.sandboxLock("sbx_1")
	lock.Lock()
	r.reconcileSandboxMaterial(t.Context(), logger)
	lock.Unlock()
	if !sentinelsRegistered(t, r, "sbx_1") {
		t.Fatal("the reaper removed the sentinels a create holding the sandbox's lock had registered")
	}

	// Nothing holds it now, and there is still no container: an orphan.
	r.reconcileSandboxMaterial(t.Context(), logger)
	if sentinelsRegistered(t, r, "sbx_1") {
		t.Fatal("an orphan's sentinels were kept")
	}
	if _, err := os.Stat(filepath.Join(proxyagent.PoolSandboxMaterialRoot(r.root, r.projectID, r.poolID), "sbx_1")); !os.IsNotExist(err) {
		t.Fatalf("an orphan's material record was kept, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.root.ProxyCerts(r.projectID, r.poolID), "clients", "sbx_1")); !os.IsNotExist(err) {
		t.Fatalf("an orphan's client certificate was kept, stat err = %v", err)
	}
}

// A snapshot build has no sandbox container to be found by, and its material
// outlives an agent that restarted under it, so the reaper leaves it to the
// snapshot sweep: dropped while its container runs, its egress would be
// unrestricted.
func TestMaterialReaperLeavesASnapshotBuildToTheSweep(t *testing.T) {
	r := materialDaemon(t, "sbx_1", 1<<62)
	id := snapshotBuildID(strings.Repeat("a", 64))
	stageAgedMaterial(t, r, id)
	r.reconcileSandboxMaterial(t.Context(), slog.New(slog.DiscardHandler))
	if _, err := os.Stat(filepath.Join(proxyagent.PoolSandboxMaterialRoot(r.root, r.projectID, r.poolID), id)); err != nil {
		t.Fatalf("the reaper removed a build's material: %v", err)
	}
}

// The listing the scan starts from can predate a create that finished before
// the reaper took the lock. The look taken under the lock is what decides.
func TestMaterialReaperSparesASandboxWhoseContainerAppearedAfterTheScan(t *testing.T) {
	r := materialDaemon(t, "sbx_1", 1)
	stageAgedMaterial(t, r, "sbx_1")

	r.reconcileSandboxMaterial(t.Context(), slog.New(slog.DiscardHandler))
	if !sentinelsRegistered(t, r, "sbx_1") {
		t.Fatal("the reaper removed the sentinels of a sandbox whose container existed when it reaped")
	}
}

// A sentinel push for a sandbox with no container writes nothing: it may have
// raced a delete, and an entry left with no material record naming it is one
// the reaper never finds. The create that builds a container carries the set.
func TestSentinelUpdateWithoutAContainerWritesNothing(t *testing.T) {
	r := materialDaemon(t, "sbx_1", 1<<62)

	_, err := r.UpdateSandbox(t.Context(), "sbx_1", &workerapimodel.PoolSandboxUpdateRequest{
		Sentinels: workerclient.NewOptNilStringArray([]string{"sk-sbx_1"}),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateSandbox() error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(r.root.ProxySecretsFile(r.projectID, r.poolID)); err == nil && sentinelsRegistered(t, r, "sbx_1") {
		t.Fatal("a sentinel push for a sandbox with no container registered its sentinels")
	}
}
