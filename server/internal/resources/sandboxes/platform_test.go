package sandboxes

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A sandbox runs on its pool's platform, which its harness's image must be
// published for (ADR 0145 §1). Create refuses a pool of a platform the image is
// not published for — what a single-platform development build meets on a
// cross-architecture pool — with a 409 that says what the image is published
// for and what the pool hosts. A pool that has not declared one yet is still a
// target, and the sandbox has no platform until it is placed. A multi-platform
// image runs on a pool of any platform it publishes.
func TestCreateRefusesAPoolOfAnotherPlatform(t *testing.T) {
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

	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", riscv, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	err := create("mismatched")
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("err = %v, want a 409", err)
	}
	if !strings.Contains(err.Error(), riscv.String()) || !strings.Contains(err.Error(), platform.Pool().String()+" only") {
		t.Fatalf("err = %v, want it to say what the image is published for and what the pool hosts", err)
	}

	// The release image is published for both, so the same pool runs it.
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

	if _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), true, true, false, 1, 1, 1, nil); err != nil {
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
