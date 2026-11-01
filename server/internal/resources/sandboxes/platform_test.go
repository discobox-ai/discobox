package sandboxes

import (
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A sandbox runs on its pool's platform, and its harness rules none out: a
// harness is a bundle, whose steps select the platform they run on (ADR
// 26-10-16-423 §2, ADR 26-11-01-122). A pool that has not declared one yet is
// still a target, and the sandbox has no platform until it is placed.
func TestCreateTakesThePoolsPlatform(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	var created *model.Sandbox
	create := func(name string) error {
		var err error
		created, err = svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("codex"),
			Config:      serverapi.SandboxCreateConfig{Name: name},
		})
		return err
	}

	if err := create("undeclared"); err != nil {
		t.Fatalf("create on a pool that has not declared a platform: %v", err)
	}
	if !created.Platform.IsZero() {
		t.Fatalf("platform = %q before the pool declared one, want none", created.Platform)
	}
	for _, hosts := range []platform.Platform{{OS: "linux", Arch: "riscv64"}, platform.Pool()} {
		if _, err := st.UpdatePoolStatus(ctx, "pool-1", hosts, platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
			t.Fatal(err)
		}
		if err := create("on-" + hosts.Arch); err != nil {
			t.Fatalf("create on a %s pool: %v", hosts, err)
		}
		sb, err := st.GetSandbox(ctx, "project-1", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sb.Platform != hosts {
			t.Fatalf("platform = %q, want the pool's %q", sb.Platform, hosts)
		}
	}
}

// A harness names no image kind: a sandbox on it is the kind of box its pool
// runs (ADR 26-11-01-122). The same bundle harness is created on a boxd pool
// and on a Docker pool of the same platform, where a harness stamped OCI was
// refused on the boxd pool with a 409 (issue 426).
func TestCreateTakesThePoolsImageKind(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "shell", "Shell")
	if err := st.CreatePool(ctx, &model.Pool{
		ID: "pool-boxd", ProjectID: "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-boxd", ProviderInstanceID: "provider-1"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdatePoolStatus(ctx, "pool-boxd", platform.Pool(), platform.VM("boxd"), true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	for _, pool := range []string{"pool-boxd", "pool-1"} {
		created, err := svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("shell"),
			PoolId:      serverapi.NewOptString(pool),
			Config:      serverapi.SandboxCreateConfig{Name: "on-" + pool},
		})
		if err != nil {
			t.Fatalf("create on %s: %v", pool, err)
		}
		if created.PoolID != pool || created.Platform != platform.Pool() || created.HarnessPin().Digest == "" {
			t.Fatalf("created on %q as %q pinned to %+v, want %s as %q on its bundle", created.PoolID, created.Platform, created.HarnessPin(), pool, platform.Pool())
		}
	}
}
