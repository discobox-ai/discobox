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

// A create sent again after the agent was replaced under the first finds the
// container that first attempt built and never started. Asked to start, it
// finishes the job — the container is rebuilt and started — instead of
// answering with the stopped one as if the create were done. Not asked to,
// the container it finds is the answer, as for any create that finds one; and
// a container that has run and was stopped is a stop, which a create does not
// undo, however often a parked sandbox's reconcile asks it to start.
func TestACreateAskedToStartFinishesOneThatNeverStartedItsContainer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		start       workerclient.OptBool
		wantRebuild bool
	}{
		{"asked to start", "created", workerclient.NewOptBool(true), true},
		{"not asked to start", "created", workerclient.NewOptBool(false), false},
		{"start not said", "created", workerclient.OptBool{}, false},
		{"stopped after running", "exited", workerclient.NewOptBool(true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := withTestRoot(t)
			var mu sync.Mutex
			var pulled bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Api-Version", "1.45")
				path := r.URL.Path
				switch {
				case strings.HasSuffix(path, "/_ping"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/json"):
					_, _ = w.Write([]byte(`[{"Id":"ctr_built","Labels":{}}]`))
				case r.Method == http.MethodGet && strings.HasSuffix(path, "/containers/ctr_built/json"):
					_, _ = w.Write([]byte(`{"Id":"ctr_built","Image":"sha256:same","State":{"Status":"` + tc.state + `"},"Config":{"Labels":{"` + sandboxLabelSpec + `":"spec-1"}}}`))
				case strings.Contains(path, "/images/") && strings.HasSuffix(path, "/json"),
					strings.HasSuffix(path, "/images/create"):
					// The rebuild's first step; refusing it ends the create
					// there, which is all this needs to see.
					mu.Lock()
					pulled = true
					mu.Unlock()
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"message":"not found"}`))
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

			sb, err := r.CreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{
				SandboxId: "sbx_1",
				Config: workerapimodel.SandboxConfig{
					Image:           workerclient.NewOptString("discobox/harness:same"),
					SpecFingerprint: workerclient.NewOptString("spec-1"),
					Start:           tc.start,
				},
				ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerclient.ResolvedHarnessConfig{}),
			})
			mu.Lock()
			defer mu.Unlock()
			if pulled != tc.wantRebuild {
				t.Fatalf("rebuilt = %v, want %v (CreateSandbox = %v, %v)", pulled, tc.wantRebuild, sb, err)
			}
			if !tc.wantRebuild && (err != nil || sb == nil || sb.ID != "ctr_built") {
				t.Fatalf("CreateSandbox = %#v, %v; want the container it found", sb, err)
			}
		})
	}
}
