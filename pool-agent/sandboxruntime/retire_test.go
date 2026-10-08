package sandboxruntime

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/client"
)

// labelDaemon answers an inspect of container "ctr_1" with labels, and records
// whether the container was removed.
func labelDaemon(t *testing.T, labels map[string]string) (*DockerSandboxRuntime, *atomic.Bool) {
	t.Helper()
	var removed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/ctr_1/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "ctr_1", "Config": map[string]any{"Labels": labels}})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/ctr_1"):
			removed.Store(true)
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
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli, identityKey: key}, &removed
}

// A container from before the bootstrap named the pool's key cannot take a
// runtime-config document, so its next start retires it for the control plane
// to rebuild; one that can is started as it is (ADR 26-10-08-127).
func TestStartRetiresAContainerThatCannotTakeRuntimeConfig(t *testing.T) {
	sb := &Sandbox{ID: "ctr_1", SandboxID: "sbx_1", Status: StatusStopped}

	old, removed := labelDaemon(t, map[string]string{sandboxLabelSandbox: "sbx_1"})
	err := old.retireContainerWithoutRuntimeConfig(context.Background(), sb)
	if !errors.Is(err, errContainerRetired) || !errors.Is(err, ErrNoContainer) {
		t.Fatalf("starting a container from before the bootstrap: %v, want it retired as no-container", err)
	}
	if !removed.Load() {
		t.Fatal("the container was not removed for the control plane to rebuild")
	}

	current, removed := labelDaemon(t, map[string]string{sandboxLabelSandbox: "sbx_1", sandboxLabelRuntimeConfig: "true"})
	if err := current.retireContainerWithoutRuntimeConfig(context.Background(), sb); err != nil {
		t.Fatalf("starting a container that takes runtime config: %v", err)
	}
	if removed.Load() {
		t.Fatal("a container that takes runtime config was removed")
	}
}
