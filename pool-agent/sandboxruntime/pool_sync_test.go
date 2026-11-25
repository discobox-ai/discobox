package sandboxruntime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// poolSyncContainer is one container a shared host daemon holds for some pool.
type poolSyncContainer struct {
	name    string
	labels  map[string]string
	created time.Time
}

// newPoolSyncDaemon is a shared Docker daemon holding the given containers,
// listing them by label and name filter and removing them on request, with
// pool_a's runtime on it.
func newPoolSyncDaemon(t *testing.T, containers map[string]poolSyncContainer) (*DockerSandboxRuntime, func() []string) {
	t.Helper()
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		mu.Lock()
		defer mu.Unlock()
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
			for id, ctr := range containers {
				match := true
				for want := range filters["label"] {
					key, value, _ := strings.Cut(want, "=")
					if ctr.labels[key] != value {
						match = false
					}
				}
				for want := range filters["name"] {
					if !strings.Contains(ctr.name, want) {
						match = false
					}
				}
				if match {
					items = append(items, map[string]any{"Id": id, "Names": []string{"/" + ctr.name}, "Labels": ctr.labels, "Created": ctr.created.Unix()})
				}
			}
			_ = json.NewEncoder(w).Encode(items)
		case r.Method == http.MethodDelete && strings.Contains(path, "/containers/"):
			id := path[strings.LastIndex(path, "/")+1:]
			if _, ok := containers[id]; !ok {
				http.NotFound(w, r)
				return
			}
			delete(containers, id)
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
	left := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for id := range containers {
			out = append(out, id)
		}
		slices.Sort(out)
		return out
	}
	return testDockerRuntime(&DockerSandboxRuntime{client: cli, root: withTestRoot(t), projectID: "proj_a", poolID: "pool_a"}), left
}

// The containers of an orphan made long ago, and of a sibling pool made after
// the control plane read the pool set: a sandbox container and a snapshot
// build each.
func poolSyncContainers(listReadAt time.Time) map[string]poolSyncContainer {
	sandboxLabels := func(poolID string) map[string]string {
		return map[string]string{sandboxLabelManaged: "true", sandboxLabelProject: "proj_a", sandboxLabelPool: poolID, sandboxLabelSandbox: "sbx_" + poolID}
	}
	buildLabels := func(poolID string) map[string]string {
		return map[string]string{sandboxLabelProject: "proj_a", sandboxLabelPool: poolID}
	}
	old := listReadAt.Add(-time.Hour)
	fresh := listReadAt.Add(time.Second)
	return map[string]poolSyncContainer{
		"ctr-orphan":   {name: "sbx-orphan", labels: sandboxLabels("pool_orphan"), created: old},
		"build-orphan": {name: snapshotContainerPrefix + "orphan", labels: buildLabels("pool_orphan"), created: old},
		"ctr-new":      {name: "sbx-new", labels: sandboxLabels("pool_new"), created: fresh},
		"build-new":    {name: snapshotContainerPrefix + "new", labels: buildLabels("pool_new"), created: fresh},
	}
}

// A sibling pool whose row commits after the control plane read the pool set
// keeps its sandbox containers and snapshot builds through the sync that set
// drives; an orphan's, made before the read, are removed.
func TestSyncKnownPoolsSparesContainersOfAPoolCreatedAfterTheListWasRead(t *testing.T) {
	listReadAt := time.Now().Add(-3 * time.Second)
	r, left := newPoolSyncDaemon(t, poolSyncContainers(listReadAt))

	if err := r.SyncKnownPools(t.Context(), []string{"pool_a"}, listReadAt); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, want := left(), []string{"build-new", "ctr-new"}; !slices.Equal(got, want) {
		t.Fatalf("containers left = %v, want %v", got, want)
	}
}

// A control plane that sends no age gets the sync it always did: every pool
// the set does not name is reaped.
func TestSyncKnownPoolsWithNoListTimeReapsEveryUnknownPool(t *testing.T) {
	r, left := newPoolSyncDaemon(t, poolSyncContainers(time.Now().Add(-3*time.Second)))

	if err := r.SyncKnownPools(t.Context(), []string{"pool_a"}, time.Time{}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got := left(); len(got) != 0 {
		t.Fatalf("containers left = %v, want none", got)
	}
}
