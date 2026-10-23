package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// asManifestFile turns config into a template with no image: its manifest a
// file, which says the platforms and the kind of box it runs on, where a
// bundle, applied onto a pool's own sandbox base image, says neither.
func asManifestFile(t *testing.T, st *store.Store, config *model.HarnessConfig, platforms ...platform.Platform) {
	t.Helper()
	config.Bundle, config.BundleDigest = "", ""
	config.Image, config.ImageDigest = "file:///etc/discobox/harnesses/"+config.Slug+".json", legacyImageDigest
	config.Platforms = platform.NewSet(platforms...)
	if err := st.UpdateHarnessConfig(context.Background(), config); err != nil {
		t.Fatal(err)
	}
}

// A sandbox runs on its pool's platform, which a template with no image must
// run on (ADR 0145 §1). Create refuses a pool of a platform the manifest file
// does not declare with a 409 that says what it runs on and what the pool
// hosts. A pool that has not declared one yet is still a target, and the
// sandbox has no platform until it is placed. A template of several platforms
// runs on a pool of any of them, and a harness bundle on a pool of any.
func TestCreateRefusesAPoolOfAnotherPlatform(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "bundled", "Bundled")
	asManifestFile(t, st, configuredHarness(t, st, "codex", "Codex"), platform.Pool())
	var created *model.Sandbox
	createOn := func(harness, name string) error {
		var err error
		created, err = svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString(harness),
			Config:      serverapi.SandboxCreateConfig{Name: name},
		})
		return err
	}
	create := func(name string) error { return createOn("codex", name) }

	if err := create("undeclared"); err != nil {
		t.Fatalf("create on a pool that has not declared a platform: %v", err)
	}
	if !created.Platform.IsZero() {
		t.Fatalf("platform = %q before the pool declared one, want none", created.Platform)
	}

	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", riscv, platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	err := create("mismatched")
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("err = %v, want a 409", err)
	}
	if !strings.Contains(err.Error(), riscv.String()) || !strings.Contains(err.Error(), platform.Pool().String()+" only") {
		t.Fatalf("err = %v, want it to say what the template runs on and what the pool hosts", err)
	}
	// A bundle declares no platform: the pool's base image is its own.
	if err := createOn("bundled", "bundled-cross-arch"); err != nil {
		t.Fatalf("create of a harness bundle on a pool of another platform: %v", err)
	}

	// A template of both platforms runs on the same pool.
	harness, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatal(err)
	}
	harness.Platforms = platform.NewSet(platform.Pool(), riscv)
	if err := st.UpdateHarnessConfig(ctx, harness); err != nil {
		t.Fatal(err)
	}
	if err := create("cross-arch"); err != nil {
		t.Fatalf("create of a multi-platform harness on a pool of another of its platforms: %v", err)
	}
	if created.Platform != riscv {
		t.Fatalf("platform = %q, want the pool's %q", created.Platform, riscv)
	}

	if _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := create("matched"); err != nil {
		t.Fatalf("create on its own platform: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Platform != platform.Pool() {
		t.Fatalf("platform = %q, want the pool's %q", sb.Platform, platform.Pool())
	}
}

// A pool runs one kind of image beside its one platform, and create refuses a
// harness whose image is another kind (ADR 26-10-09-106 §4): a disco-vm harness
// for boxd on a Docker pool of the very platform its image is published for is
// a 409 that names both kinds, and the same harness is created on a discovm
// pool of boxd. A pool whose agent has not said what it runs is still a target.
func TestCreateRefusesAPoolOfAnotherImageKind(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	machine := configuredHarness(t, st, "machine", "Machine")
	asManifestFile(t, st, machine)
	machine.ImageKind = platform.DiscoVM("boxd")
	if err := st.UpdateHarnessConfig(ctx, machine); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePool(ctx, &model.Pool{
		ID: "pool-boxd", ProjectID: "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-boxd", ProviderInstanceID: "provider-1"},
	}); err != nil {
		t.Fatal(err)
	}
	create := func(name, pool string) (*model.Sandbox, error) {
		return svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("machine"),
			PoolId:      serverapi.NewOptString(pool),
			Config:      serverapi.SandboxCreateConfig{Name: name},
		})
	}

	if _, err := create("undeclared", "pool-boxd"); err != nil {
		t.Fatalf("create on a pool that has not declared what it runs: %v", err)
	}

	if _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	_, err := create("on-docker", "pool-1")
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("err = %v, want a 409", err)
	}
	if !strings.Contains(err.Error(), "a disco-vm image for the boxd driver, and the pool runs OCI images") {
		t.Fatalf("err = %v, want it to say both kinds", err)
	}

	if _, err := st.UpdatePoolStatus(ctx, "pool-boxd", platform.Pool(), platform.DiscoVM("boxd"), true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	created, err := create("on-boxd", "pool-boxd")
	if err != nil {
		t.Fatalf("create on a discovm pool of its driver: %v", err)
	}
	if created.PoolID != "pool-boxd" || created.Platform != platform.Pool() {
		t.Fatalf("created on %q as %q, want pool-boxd as %q", created.PoolID, created.Platform, platform.Pool())
	}
}
