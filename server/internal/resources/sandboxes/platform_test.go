package sandboxes

import (
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
		if _, _, err := st.UpdatePoolStatus(ctx, "pool-1", hosts, platform.OCI, store.AgentSentinels{}, true, true, false, 1, 1, 1, nil); err != nil {
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
	if _, _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, store.AgentSentinels{}, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpdatePoolStatus(ctx, "pool-boxd", platform.Pool(), platform.VM("boxd"), store.AgentSentinels{}, true, true, false, 1, 1, 1, nil); err != nil {
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

// The account a create names is judged by the ids its pool's guest gives
// accounts: a macOS client's own 501 in staff is created on a darwin pool and
// refused on a Linux one, and a pool that has not declared a platform is
// judged as Linux (ADR 26-11-12-060).
func TestCreateJudgesTheUserByThePoolsPlatform(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	macAccount := serverapi.SandboxUser{
		Name: serverapi.NewOptString("ada"),
		UID:  serverapi.NewOptInt64(501),
		Gid:  serverapi.NewOptInt64(20),
	}
	create := func(name string) error {
		_, err := svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("codex"),
			Config: serverapi.SandboxCreateConfig{
				Name: name,
				User: serverapi.NewOptSandboxUser(macAccount),
			},
		})
		return err
	}

	if err := create("undeclared"); err == nil || !strings.Contains(err.Error(), "uid 501") {
		t.Fatalf("501/20 on a pool with no platform = %v, want uid 501 refused as Linux refuses it", err)
	}
	for _, tc := range []struct {
		hosts platform.Platform
		ok    bool
	}{
		{platform.Platform{OS: "linux", Arch: "arm64"}, false},
		{platform.Platform{OS: "darwin", Arch: "arm64"}, true},
	} {
		if _, _, err := st.UpdatePoolStatus(ctx, "pool-1", tc.hosts, platform.VM("vz"), store.AgentSentinels{}, true, true, false, 1, 1, 1, nil); err != nil {
			t.Fatal(err)
		}
		err := create("on-" + tc.hosts.OS)
		if tc.ok && err != nil {
			t.Fatalf("501/20 on a %s pool: %v", tc.hosts, err)
		}
		if !tc.ok {
			var statusErr interface{ StatusCode() int }
			if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "uid 501") {
				t.Fatalf("501/20 on a %s pool = %v, want a 400 naming uid 501", tc.hosts, err)
			}
		}
	}
}
