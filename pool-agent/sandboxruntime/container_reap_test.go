package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// containerDaemon is a Docker daemon holding one container per sandbox ID
// given, labeled as this pool's runtime labels its sandboxes' containers. It
// lists them by label filter and removes them on request.
type containerDaemon struct {
	mu         sync.Mutex
	containers map[string]string // container ID -> sandbox ID label
}

func (d *containerDaemon) sandboxes() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, sandboxID := range d.containers {
		out = append(out, sandboxID)
	}
	slices.Sort(out)
	return out
}

func newContainerDaemon(t *testing.T, sandboxIDs ...string) (*DockerSandboxRuntime, *containerDaemon) {
	t.Helper()
	d := &containerDaemon{containers: map[string]string{}}
	for _, sandboxID := range sandboxIDs {
		d.containers["ctr-"+sandboxID] = sandboxID
	}
	const projectID, poolID = "proj_a", "pool_a"
	labels := func(sandboxID string) map[string]string {
		return map[string]string{
			sandboxLabelManaged: "true",
			sandboxLabelProject: projectID,
			sandboxLabelPool:    poolID,
			sandboxLabelSandbox: sandboxID,
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		d.mu.Lock()
		defer d.mu.Unlock()
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/json"):
			var filters map[string]map[string]bool
			if raw := r.URL.Query().Get("filters"); raw != "" {
				if err := json.Unmarshal([]byte(raw), &filters); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
			}
			items := []map[string]any{}
			for ctrID, sandboxID := range d.containers {
				have := labels(sandboxID)
				match := true
				for want := range filters["label"] {
					key, value, _ := strings.Cut(want, "=")
					if have[key] != value {
						match = false
					}
				}
				if match {
					items = append(items, map[string]any{"Id": ctrID, "Labels": have})
				}
			}
			_ = json.NewEncoder(w).Encode(items)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/json"):
			ctrID := path[strings.LastIndex(strings.TrimSuffix(path, "/json"), "/")+1 : len(path)-len("/json")]
			sandboxID, ok := d.containers[ctrID]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": ctrID, "Config": map[string]any{"Labels": labels(sandboxID)}, "State": map[string]any{"Status": "created"}})
		case r.Method == http.MethodDelete && strings.Contains(path, "/containers/"):
			ctrID := path[strings.LastIndex(path, "/")+1:]
			if _, ok := d.containers[ctrID]; !ok {
				http.NotFound(w, r)
				return
			}
			delete(d.containers, ctrID)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return testDockerRuntime(&DockerSandboxRuntime{client: cli, root: withTestRoot(t), projectID: projectID, poolID: poolID}), d
}

// A sandbox whose delete never reached this pool keeps its tree and its
// container. Once the control plane has not held it for the retention, both
// go: the container is what pins its image, and left behind it is never
// removed.
func TestDockerReaperRemovesAnUnheldSandboxsContainerWithItsTree(t *testing.T) {
	r, daemon := newContainerDaemon(t, "sbx_gone", "sbx_held")
	for _, sandboxID := range []string{"sbx_gone", "sbx_held"} {
		mkSandboxDir(t, r.sandboxesRoot(), sandboxID)
	}
	ctx := context.Background()
	now := time.Now()
	held := heldSet("sbx_held")

	r.reapUnheldSandboxes(ctx, quietLogger(), held, now)
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"sbx_gone", "sbx_held"}) {
		t.Fatalf("containers after the first pass = %v, want both kept", got)
	}
	r.reapUnheldSandboxes(ctx, quietLogger(), held, now.Add(sandboxVolumeRetention+time.Minute))
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"sbx_held"}) {
		t.Fatalf("containers after retention = %v, want only the unheld one removed", got)
	}
	if _, err := os.Stat(r.sandboxRoot("sbx_gone")); !os.IsNotExist(err) {
		t.Fatalf("unheld sandbox's tree after retention: stat err = %v, want it gone", err)
	}
	if _, err := os.Stat(r.sandboxRoot("sbx_held")); err != nil {
		t.Fatalf("held sandbox's tree: %v", err)
	}
	// A reap is not the owner's word that the data should go: the trail is
	// left to the proxy's age sweep (ADR 26-11-14-896 §5).
	if purged := r.audit.(*fakeAuditPurger).purges(); len(purged) != 0 {
		t.Fatalf("the reaper purged audit trails %v, want none", purged)
	}
}

// A delete is a purge: it takes the sandbox's audit trail from the pool
// proxy with everything else, after the container, and a proxy that cannot
// take it fails the delete with the tree still there, so the purge's
// reconcile retries it (ADR 26-11-14-896 §5, ADR 0022 §3).
func TestDockerDeletePurgesTheAuditTrailOrFails(t *testing.T) {
	r, daemon := newContainerDaemon(t, "sbx_gone")
	mkSandboxDir(t, r.sandboxesRoot(), "sbx_gone")
	ctx := context.Background()
	audit := r.audit.(*fakeAuditPurger)

	audit.err = errors.New("proxy control API unreachable")
	if err := r.DeleteSandbox(ctx, "sbx_gone"); err == nil {
		t.Fatal("a delete whose audit purge failed succeeded")
	}
	if got := daemon.sandboxes(); len(got) != 0 {
		t.Fatalf("containers after the failed delete = %v, want the container gone first", got)
	}
	if _, err := os.Stat(r.sandboxRoot("sbx_gone")); err != nil {
		t.Fatalf("the tree after a failed purge: %v, want it kept for the retry", err)
	}

	audit.err = nil
	if err := r.DeleteSandbox(ctx, "sbx_gone"); err != nil {
		t.Fatalf("the retried delete: %v", err)
	}
	if purged := audit.purges(); !slices.Equal(purged, []string{"sbx_gone"}) {
		t.Fatalf("audit trails purged = %v, want the sandbox's", purged)
	}
	if _, err := os.Stat(r.sandboxRoot("sbx_gone")); !os.IsNotExist(err) {
		t.Fatalf("the tree after the delete: stat err = %v, want it gone", err)
	}
}

// A container whose tree is already gone — reaped by a pool from before the
// reaper took containers with trees — is named by nothing the tree reaper
// sees. It is removed once neither a tree nor a held sandbox has named it for
// the retention; a held one is kept, and so is a container with no sandbox's
// ID.
func TestDockerReaperRemovesContainersNoSandboxNames(t *testing.T) {
	r, daemon := newContainerDaemon(t, "sbx_orphan", "sbx_held", "sbx_kept", "not-a-sandbox")
	mkSandboxDir(t, r.sandboxesRoot(), "sbx_kept")
	ctx := context.Background()
	now := time.Now()
	held := heldSet("sbx_held", "sbx_kept")

	r.reapOrphanContainers(ctx, quietLogger(), held, now)
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"not-a-sandbox", "sbx_held", "sbx_kept", "sbx_orphan"}) {
		t.Fatalf("containers after the first pass = %v, want every one kept", got)
	}
	r.reapOrphanContainers(ctx, quietLogger(), held, now.Add(sandboxVolumeRetention-time.Minute))
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"not-a-sandbox", "sbx_held", "sbx_kept", "sbx_orphan"}) {
		t.Fatalf("containers inside the retention = %v, want every one kept", got)
	}
	r.reapOrphanContainers(ctx, quietLogger(), held, now.Add(sandboxVolumeRetention+time.Minute))
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"not-a-sandbox", "sbx_held", "sbx_kept"}) {
		t.Fatalf("containers after retention = %v, want only the orphan removed", got)
	}
	if entries, _ := os.ReadDir(r.root.PoolData(r.projectID, r.poolID) + "/" + containerOrphanMarkersName); len(entries) != 0 {
		t.Fatalf("orphan markers left after the orphan went: %d", len(entries))
	}
	if purged := r.audit.(*fakeAuditPurger).purges(); len(purged) != 0 {
		t.Fatalf("the orphan sweep purged audit trails %v, want none", purged)
	}
}

// An orphan held again before its retention ends starts no clock: one held
// for a moment later is judged from that moment, not from the first sighting.
// And a pass with no answer from the control plane removes nothing.
func TestDockerReaperOrphanContainerClockAndNoAnswer(t *testing.T) {
	r, daemon := newContainerDaemon(t, "sbx_back")
	ctx := context.Background()
	now := time.Now()

	r.reapOrphanContainers(ctx, quietLogger(), heldSet(), now)
	r.reapOrphanContainers(ctx, quietLogger(), heldSet("sbx_back"), now.Add(time.Hour))
	r.reapOrphanContainers(ctx, quietLogger(), heldSet(), now.Add(2*time.Hour))
	r.reapOrphanContainers(ctx, quietLogger(), heldSet(), now.Add(sandboxVolumeRetention+time.Minute))
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"sbx_back"}) {
		t.Fatalf("containers = %v, want the one held again kept until its new clock runs out", got)
	}

	noAnswer := func(context.Context) ([]string, error) { return nil, errors.New("control plane unreachable") }
	r.reapOrphanContainers(ctx, quietLogger(), noAnswer, now.Add(3*sandboxVolumeRetention))
	if got := daemon.sandboxes(); !slices.Equal(got, []string{"sbx_back"}) {
		t.Fatalf("containers after a pass with no answer = %v, want it kept", got)
	}
	r.reapOrphanContainers(ctx, quietLogger(), heldSet(), now.Add(3*sandboxVolumeRetention))
	if got := daemon.sandboxes(); len(got) != 0 {
		t.Fatalf("containers after the new clock ran out = %v, want none", got)
	}
}
