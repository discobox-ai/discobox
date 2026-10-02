package sandboxruntime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// A spec change that cannot get its new image must leave the sandbox the
// container it had. Removing it first left a sandbox with a tree and no
// container, which a settled failure keeps for as long as its repair takes
// (ADR 26-10-01-876 §4).
func TestASpecChangeThatCannotGetItsImageKeepsTheContainer(t *testing.T) {
	state := withTestRoot(t)
	var mu sync.Mutex
	var removed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/json"):
			_, _ = w.Write([]byte(`[{"Id":"ctr_old","Labels":{}}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/ctr_old/json"):
			_, _ = w.Write([]byte(`{"Id":"ctr_old","Image":"sha256:old","State":{"Status":"exited"},"Config":{"Labels":{"` + sandboxLabelSpec + `":"spec-old"}}}`))
		case strings.Contains(path, "/images/") && strings.HasSuffix(path, "/json"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
		case strings.HasSuffix(path, "/images/create"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"docker.io/discobox/harness:gone: not found"}`))
		case r.Method == http.MethodDelete || strings.HasSuffix(path, "/stop"):
			mu.Lock()
			removed = append(removed, r.Method+" "+path)
			mu.Unlock()
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
	r := &DockerSandboxRuntime{root: state, client: cli, projectID: "proj_1", poolID: "pool_1"}

	_, err = r.CreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sbx_1",
		Config: workerapimodel.SandboxConfig{
			Image:           workerclient.NewOptString("discobox/harness:gone"),
			SpecFingerprint: workerclient.NewOptString("spec-new"),
		},
		ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerclient.ResolvedHarnessConfig{}),
	})
	if !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("CreateSandbox = %v, want ErrImageUnavailable", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(removed) != 0 {
		t.Fatalf("the existing container was torn down before the new image was in hand: %v", removed)
	}
}
