package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	api "github.com/discobox-ai/discobox/pool-agent/api/gen"
	"github.com/moby/moby/client"
)

func TestHarnessChangeFenceSurvivesRestartAndBlocksOldCreates(t *testing.T) {
	for _, state := range []string{"running", "paused", "exited"} {
		t.Run(state, func(t *testing.T) {
			root := withTestRoot(t)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Api-Version", "1.45")
				switch {
				case strings.HasSuffix(req.URL.Path, "/_ping"):
					w.WriteHeader(200)
				case strings.HasSuffix(req.URL.Path, "/containers/json"):
					_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "ctr_old"}})
				case strings.HasSuffix(req.URL.Path, "/containers/ctr_old/logs"):
					w.WriteHeader(200)
				case strings.HasSuffix(req.URL.Path, "/containers/ctr_old/json"):
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "ctr_old", "State": map[string]any{"Status": state, "Running": state == "running" || state == "paused", "Paused": state == "paused"}, "Config": map[string]any{"Labels": map[string]string{sandboxLabelSandbox: "sbx_1"}}})
				default:
					t.Errorf("unexpected runtime mutation: %s %s", req.Method, req.URL.Path)
					http.NotFound(w, req)
				}
			}))
			defer daemon.Close()
			docker, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(daemon.URL, "http://")), client.WithHTTPClient(daemon.Client()))
			if err != nil {
				t.Fatal(err)
			}
			runtime := &DockerSandboxRuntime{root: root, client: docker, projectID: "proj_1", poolID: "pool_1"}
			if err := os.MkdirAll(runtime.sandboxRoot("sbx_1"), 0700); err != nil {
				t.Fatal(err)
			}
			_, err = runtime.reserveHarnessChange(context.Background(), "sbx_1", "change-1")
			if state != "exited" {
				if !errors.Is(err, ErrHarnessChangeRefused) {
					t.Fatalf("reserve %s: %v", state, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Reconstruct the pool object, with no in-memory locks or reservation state.
			restarted := &DockerSandboxRuntime{root: root, client: docker, projectID: "proj_1", poolID: "pool_1"}
			if err := restarted.StartSandbox(context.Background(), "sbx_1", &api.PoolSandboxOperationRequest{}); !errors.Is(err, ErrHarnessChangeRefused) {
				t.Fatalf("start through persisted fence: %v", err)
			}
			if err := restarted.harnessCreateAllowed("sbx_1", ""); !errors.Is(err, ErrHarnessChangeRefused) {
				t.Fatalf("old create: %v", err)
			}
			if _, err := restarted.reserveHarnessChange(context.Background(), "sbx_1", "change-1"); err != nil {
				t.Fatalf("retry reservation: %v", err)
			}
			if err := restarted.harnessCreateAllowed("sbx_1", "change-1"); err != nil {
				t.Fatal(err)
			}
			if err := restarted.finishHarnessChange("sbx_1", "change-1"); err != nil {
				t.Fatal(err)
			}
			if err := restarted.harnessStartAllowed("sbx_1"); err != nil {
				t.Fatalf("start after apply: %v", err)
			}
			if err := restarted.harnessCreateAllowed("sbx_1", ""); !errors.Is(err, ErrHarnessChangeRefused) {
				t.Fatalf("late old create: %v", err)
			}
		})
	}
}

func TestFailedArchiveKeepsHarnessFence(t *testing.T) {
	r := &DockerSandboxRuntime{root: withTestRoot(t), projectID: "proj_1", poolID: "pool_1"}
	root := r.sandboxRoot("sbx_1")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.writeHarnessFence("sbx_1", harnessChangeFence{ID: "pending"}); err != nil {
		t.Fatal(err)
	}
	// Force archive's marker write to fail before any container operation.
	if err := os.Mkdir(sandboxArchiveMarkerPath(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := r.ArchiveSandbox(context.Background(), "sbx_1"); err == nil {
		t.Fatal("expected marker failure")
	}
	fence, err := r.readHarnessFence("sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if fence.ID != "pending" || fence.Applied {
		t.Fatalf("failed archive released fence: %+v", fence)
	}
}
