package sandboxruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
)

// Every create records the bundles it pins the sandbox to, even one that
// finds its container current and builds nothing: that record is all
// assets.discobox.internal answers the sandbox (ADR 26-10-16-423 §13), and a
// sandbox from before it is recorded gets one from the first create that
// reaches it. A create that pins none removes it.
func TestEveryCreateRecordsTheSandboxsBundles(t *testing.T) {
	state := withTestRoot(t)
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
			_, _ = w.Write([]byte(`{"Id":"ctr_built","Image":"sha256:same","State":{"Status":"exited"},"Config":{"Labels":{"` + sandboxLabelSpec + `":"spec-1"}}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/images/discobox-sandbox-base:test/json"):
			// The base image the container already runs: the pins moved, and
			// nothing the container is built from did.
			_, _ = w.Write([]byte(`{"Id":"sha256:same"}`))
		case strings.Contains(path, "/images/"):
			t.Errorf("the container is current, but the create reached for its image: %s %s", r.Method, path)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{root: state, client: cli, projectID: "proj_1", poolID: "pool_1", sandboxBaseImage: "discobox-sandbox-base:test"}

	platform := "sha256:" + strings.Repeat("a", 64)
	harness := "sha256:" + strings.Repeat("b", 64)
	create := func(bundles []workerapimodel.BundlePin) {
		t.Helper()
		req := &workerapimodel.PoolSandboxCreateRequest{
			SandboxId: "sbx_1",
			Config: workerapimodel.SandboxConfig{
				Image:           workerclient.NewOptString("discobox/harness:same"),
				SpecFingerprint: workerclient.NewOptString("spec-1"),
			},
			ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerclient.ResolvedHarnessConfig{}),
		}
		if bundles != nil {
			req.Bundles = workerclient.NewOptNilBundlePinArray(bundles)
		}
		if sb, err := r.CreateSandbox(context.Background(), req); err != nil || sb == nil || sb.ID != "ctr_built" {
			t.Fatalf("CreateSandbox = %#v, %v; want the container it found", sb, err)
		}
	}

	create([]workerapimodel.BundlePin{{Kind: workerclient.BundlePinKindPlatform, Name: "discobox-platform", Digest: platform}, {Kind: workerclient.BundlePinKindHarness, Name: "discobox-harness-shell", Digest: harness}})
	got, err := assetcache.ReadRecord(r.sandboxRoot("sbx_1"))
	want := []assetcache.Pin{{Kind: assetcache.KindPlatform, Name: "discobox-platform", Digest: platform}, {Kind: assetcache.KindHarness, Name: "discobox-harness-shell", Digest: harness}}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("record = %v, %v; want %v", got, err, want)
	}

	create(nil)
	if got, err := assetcache.ReadRecord(r.sandboxRoot("sbx_1")); err != nil || got != nil {
		t.Fatalf("record after a create pinning nothing = %v, %v; want none", got, err)
	}
}

// A sandbox delivered its platform runs the provider's sandbox base image,
// unpinned, whatever image it is pinned to; any other runs its pin (ADR
// 26-10-16-423 §§7, 10).
func TestADeliveredSandboxRunsTheSandboxBaseImage(t *testing.T) {
	r := &DockerSandboxRuntime{sandboxBaseImage: "discobox-sandbox-base:test"}
	req := &workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			Image:       workerclient.NewOptString("discobox/harness:v1"),
			ImageDigest: workerclient.NewOptString("sha256:pinned"),
		},
	}
	if image, digest := r.sandboxImage(req); image != "discobox/harness:v1" || digest != "sha256:pinned" {
		t.Fatalf("undelivered sandbox runs %q pinned to %q; want its own image and pin", image, digest)
	}
	req.Bundles = workerclient.NewOptNilBundlePinArray([]workerapimodel.BundlePin{{Kind: workerclient.BundlePinKindPlatform, Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)}})
	if image, digest := r.sandboxImage(req); image != "discobox-sandbox-base:test" || digest != "" {
		t.Fatalf("delivered sandbox runs %q pinned to %q; want the base image, unpinned", image, digest)
	}
}
