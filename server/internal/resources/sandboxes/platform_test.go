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

// A sandbox runs on its harness's platform and is placed only on a pool that
// hosts it (ADR 0145 §1). Create refuses a pool whose agent declared another,
// with both platforms as the reason, and records the platform on a sandbox it
// accepts. A pool that has not declared one yet is still a target: the
// provider checks it once the agent reports.
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

	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", riscv, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	err := create("mismatched")
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("err = %v, want a 409", err)
	}
	if !strings.Contains(err.Error(), riscv.String()) || !strings.Contains(err.Error(), platform.Pool().String()) {
		t.Fatalf("err = %v, want both platforms named", err)
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
		t.Fatalf("platform = %q, want the harness's %q", sb.Platform, platform.Pool())
	}
}
