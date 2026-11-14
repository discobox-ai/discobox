package sandboxruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/harness"
)

// dfDaemon answers `docker system df` with the containers and images given.
func dfDaemon(t *testing.T, containers, images []map[string]any, totalImages int64) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/system/df"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ContainerUsage": map[string]any{"Items": containers},
				"ImageUsage":     map[string]any{"Items": images, "TotalSize": totalImages},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()), client.WithAPIVersion("1.52"))
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

func dfContainer(project, pool, sandboxID, image string, sizeRw int64) map[string]any {
	labels := map[string]string{sandboxLabelProject: project, sandboxLabelPool: pool}
	if sandboxID != "" {
		labels[sandboxLabelManaged] = "true"
		labels[sandboxLabelSandbox] = sandboxID
	}
	return map[string]any{"Id": "c-" + sandboxID + image, "Labels": labels, "ImageID": image, "SizeRw": sizeRw}
}

// On a shared daemon only the pool's own containers count, each box's layer
// on its row and the rest as other layers; and only Discobox's images count,
// a layer they share counted once (ADR 26-11-14-896 §§1-3).
func TestDockerDiskUsageOnASharedDaemonCountsOnlyThePools(t *testing.T) {
	cli := dfDaemon(t,
		[]map[string]any{
			dfContainer("proj_1", "pool_1", "sbx_a", "sha256:base", 5),
			dfContainer("proj_1", "pool_1", "", "sha256:base", 3), // a snapshot build
			dfContainer("proj_1", "pool_2", "sbx_other", "sha256:other-pool", 100),
			{"Id": "host", "Labels": map[string]string{}, "ImageID": "sha256:host", "SizeRw": 1000},
		},
		[]map[string]any{
			{"Id": "sha256:base", "Size": 60, "SharedSize": 50},
			{"Id": "sha256:snapshot", "Size": 70, "SharedSize": 50, "Labels": map[string]string{harness.ReclaimLabel: harness.ReclaimLabelValue}},
			{"Id": "sha256:other-pool", "Size": 500, "SharedSize": 0},
			{"Id": "sha256:host", "Size": 900, "SharedSize": 0},
		}, 99999)
	r := testDockerRuntime(&DockerSandboxRuntime{client: cli, projectID: "proj_1", poolID: "pool_1"})

	usage, err := r.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Measured || !usage.DaemonShared {
		t.Errorf("usage = %+v, want measured on a shared daemon", usage)
	}
	if got := usage.Layers["sbx_a"]; got != 5 || len(usage.Layers) != 1 {
		t.Errorf("layers = %v, want only sbx_a's 5", usage.Layers)
	}
	if usage.OtherLayerBytes != 3 {
		t.Errorf("other layers = %d, want the build's 3", usage.OtherLayerBytes)
	}
	// 10 and 20 unique, and the 50 the two share once.
	if usage.ImageBytes != 80 {
		t.Errorf("images = %d, want 80", usage.ImageBytes)
	}
}

// On a dedicated daemon everything on it is the pool's: every container, its
// own included, and every image, as the daemon totals them.
func TestDockerDiskUsageOnADedicatedDaemonCountsEverything(t *testing.T) {
	cli := dfDaemon(t,
		[]map[string]any{
			dfContainer("proj_1", "pool_1", "sbx_a", "sha256:base", 5),
			{"Id": "pool-container", "Labels": map[string]string{}, "ImageID": "sha256:base", "SizeRw": 7},
		},
		[]map[string]any{{"Id": "sha256:base", "Size": 60, "SharedSize": 0}}, 4242)
	r := testDockerRuntime(&DockerSandboxRuntime{client: cli, projectID: "proj_1", poolID: "pool_1", dedicatedDaemon: true})

	usage, err := r.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.DaemonShared {
		t.Error("a dedicated daemon reported as shared")
	}
	if usage.Layers["sbx_a"] != 5 || usage.OtherLayerBytes != 7 || usage.ImageBytes != 4242 {
		t.Errorf("usage = %+v, want sbx_a 5, other layers 7, images 4242", usage)
	}
}
