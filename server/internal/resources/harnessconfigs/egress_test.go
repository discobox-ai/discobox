package harnessconfigs

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A person's harness level is stored apart from the image's hosts, converges
// the harness's discoboxes, and refuses an entry the proxy could not enforce.
func TestUpdateHarnessConfigSetsTheEgressLevel(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	config := &model.HarnessConfig{ProjectID: "project-1", Slug: "custom", Name: "Custom", ImageEgressHosts: []string{"api.example"}}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	runtime := &stubSandboxRuntime{}
	svc := &Service{store: st, sandboxes: runtime}
	level := func(allow ...string) services.UpdateHarnessConfigBody {
		current, err := st.GetHarnessConfig(ctx, "project-1", config.ID)
		if err != nil {
			t.Fatal(err)
		}
		return services.UpdateHarnessConfigBody{Revision: current.Revision, Egress: serverapi.NewOptEgressPolicy(serverapi.EgressPolicy{
			Allow: serverapi.NewOptNilStringArray(allow), DenyByDefault: serverapi.NewOptBool(true),
		})}
	}

	updated, err := svc.UpdateHarnessConfig(ctx, "project-1", config.ID, level("registry.npmjs.org"))
	if err != nil {
		t.Fatalf("update harness config: %v", err)
	}
	if !updated.Egress.DenyByDefault || !slices.Equal(updated.Egress.Allow, []string{"registry.npmjs.org"}) ||
		!slices.Equal(updated.ImageEgressHosts, []string{"api.example"}) {
		t.Fatalf("egress = %+v, image hosts = %v", updated.Egress, updated.ImageEgressHosts)
	}
	if !slices.Equal(runtime.egressChanged, []string{config.ID}) {
		t.Fatalf("converged %v, want this config's discoboxes", runtime.egressChanged)
	}

	if _, err := svc.UpdateHarnessConfig(ctx, "project-1", config.ID, services.UpdateHarnessConfigBody{Revision: updated.Revision, Name: serverapi.NewOptString("Renamed")}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(runtime.egressChanged) != 1 {
		t.Fatalf("a rename converged egress: %v", runtime.egressChanged)
	}

	_, err = svc.UpdateHarnessConfig(ctx, "project-1", config.ID, level("*.example.*"))
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("a malformed entry: err = %v, want bad request", err)
	}
}

// The hosts a bundle declares are snapshotted when it is registered, and a
// refresh that changes them converges the harness's discoboxes, leaving the
// level a person set alone.
func TestRefreshingABundleWithNewHostsConvergesItsSandboxes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "custom", Name: "Custom",
		Bundle: "registry.example/custom:v1", BundleDigest: "sha256:one",
	}
	config.Egress.Allow = []string{"github.com"}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	runtime := &stubSandboxRuntime{}
	declaring := func(digest string, hosts ...string) imageMetadata {
		return imageMetadata{Digest: digest, ImageMetadata: harness.ImageMetadata{Harness: &harness.Image{EgressHosts: hosts}}}
	}
	inspector := &stubInspector{byImage: map[string]imageMetadata{"registry.example/custom:v1": declaring("sha256:two", "API.example")}}
	svc := &Service{store: st, inspector: inspector, harnesses: testReferences, sandboxes: runtime}

	refreshed, err := svc.RefreshHarnessConfigImage(ctx, "project-1", config.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !slices.Equal(refreshed.ImageEgressHosts, []string{"api.example"}) || !slices.Equal(refreshed.Egress.Allow, []string{"github.com"}) {
		t.Fatalf("image hosts = %v, person's level = %+v", refreshed.ImageEgressHosts, refreshed.Egress)
	}
	if !slices.Equal(runtime.egressChanged, []string{config.ID}) {
		t.Fatalf("converged %v after the bundle's hosts changed", runtime.egressChanged)
	}

	inspector.byImage["registry.example/custom:v1"] = declaring("sha256:three", "api.example")
	if _, err := svc.RefreshHarnessConfigImage(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("refresh again: %v", err)
	}
	if len(runtime.egressChanged) != 1 {
		t.Fatalf("converged %v though the bundle's hosts did not change", runtime.egressChanged)
	}
}

func TestRegistrationRefusesAnEgressHostTheProxyCouldNotMatch(t *testing.T) {
	declaring := func(hosts ...string) harness.ImageMetadata {
		return harness.ImageMetadata{Harness: &harness.Image{EgressHosts: hosts}}
	}
	if err := validateImageMetadata(declaring("api.anthropic.com", "*.googleapis.com"), "linux", "test image"); err != nil {
		t.Fatalf("valid hosts refused: %v", err)
	}
	if err := validateImageMetadata(declaring("api.anthropic.com:443"), "linux", "test image"); err == nil {
		t.Fatal("a host with a port was accepted")
	}
}
