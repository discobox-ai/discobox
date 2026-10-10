package sandboxruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// Only a sandbox created in a mode other than run carries the harness-mode
// label, so a run sandbox is never found by the configure cadence.
func TestLabelsNameOnlyANonRunHarnessMode(t *testing.T) {
	r := &DockerSandboxRuntime{projectID: "prj", poolID: "pool"}
	if got := r.labels("sbx", "", "", sandboxconfig.HarnessModeConfig)[sandboxLabelHarnessMode]; got != sandboxconfig.HarnessModeConfig {
		t.Fatalf("configure sandbox label = %q, want %q", got, sandboxconfig.HarnessModeConfig)
	}
	for _, mode := range []string{"", sandboxconfig.HarnessModeRun} {
		if got, ok := r.labels("sbx", "", "", mode)[sandboxLabelHarnessMode]; ok {
			t.Fatalf("harness mode %q labeled %q, want no label", mode, got)
		}
	}
}

// The configure list is one filtered container list, scoped to this pool and
// to running containers, read back as sandbox IDs.
func TestDockerRunningSandboxIDsInModeListsLabelledRunningContainers(t *testing.T) {
	var filters map[string]map[string]bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json"):
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
				t.Errorf("decode filters: %v", err)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"Id": "ctr_1", "Labels": map[string]string{sandboxLabelSandbox: "sbx_1"}},
				{"Id": "ctr_2", "Labels": map[string]string{}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{client: cli, projectID: "prj", poolID: "pool"}

	ids, err := r.RunningSandboxIDsInMode(context.Background(), sandboxconfig.HarnessModeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"sbx_1"}) {
		t.Fatalf("ids = %v, want [sbx_1]", ids)
	}
	for _, want := range []string{sandboxLabelHarnessMode + "=config", sandboxLabelPool + "=pool", sandboxLabelProject + "=prj"} {
		if !filters["label"][want] {
			t.Errorf("label filters %v lack %q", filters["label"], want)
		}
	}
	if !filters["status"]["running"] {
		t.Errorf("status filters %v lack running", filters["status"])
	}
}

func TestMemoryRunningSandboxIDsInModeListsRunningSandboxesOfThatMode(t *testing.T) {
	ctx := context.Background()
	r := NewMemorySandboxRuntime()
	create := func(id string, mode workerclient.SandboxConfigHarnessMode) {
		t.Helper()
		config := workerclient.SandboxConfig{}
		if mode != "" {
			config.HarnessMode = workerclient.NewOptSandboxConfigHarnessMode(mode)
		}
		if _, err := r.CreateSandbox(ctx, &workerclient.PoolSandboxCreateRequest{SandboxId: id, Config: config}); err != nil {
			t.Fatal(err)
		}
	}
	create("sbx_b", workerclient.SandboxConfigHarnessModeConfig)
	create("sbx_a", workerclient.SandboxConfigHarnessModeConfig)
	create("sbx_run", workerclient.SandboxConfigHarnessModeRun)
	create("sbx_plain", "")
	create("sbx_stopped", workerclient.SandboxConfigHarnessModeConfig)
	if err := r.StopSandbox(ctx, "sbx_stopped", nil); err != nil {
		t.Fatal(err)
	}

	ids, err := r.RunningSandboxIDsInMode(ctx, sandboxconfig.HarnessModeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"sbx_a", "sbx_b"}) {
		t.Fatalf("ids = %v, want [sbx_a sbx_b]", ids)
	}

	// A re-create takes the mode it names, as the Docker runtime's labels do.
	create("sbx_b", workerclient.SandboxConfigHarnessModeRun)
	ids, err = r.RunningSandboxIDsInMode(ctx, sandboxconfig.HarnessModeConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"sbx_a"}) {
		t.Fatalf("after re-creating sbx_b in run mode, ids = %v, want [sbx_a]", ids)
	}
}
