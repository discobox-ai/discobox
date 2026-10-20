package sandboxes

import (
	"errors"
	"maps"
	"net/http"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A create's features are kept with the sandbox's spec and read back, a name
// that is not a feature's is refused with a 400, and a sandbox created with
// none keeps the fingerprint it had before features existed (ADR
// 26-10-16-423 §§4, 11).
func TestCreateKeepsItsFeatures(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	create := func(name string, features serverapi.SandboxCreateConfigFeatures) (*model.Sandbox, error) {
		config := serverapi.SandboxCreateConfig{Name: name}
		config.SetFeatures(serverapi.NewOptSandboxCreateConfigFeatures(features))
		return svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
			HarnessName: serverapi.NewOptString("codex"),
			Config:      config,
		})
	}
	created, err := create("featured", serverapi.SandboxCreateConfigFeatures{"brew": {Enabled: false}, "nix": {Enabled: true}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]sandboxconfig.Feature{"brew": {Enabled: false}, "nix": {Enabled: true}}
	if !maps.Equal(sb.Features, want) {
		t.Fatalf("stored features = %v, want %v", sb.Features, want)
	}
	read, err := services.SandboxToAPI(sb, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := read.Config.Features.Or(nil); len(got) != 2 || got["brew"].Enabled || !got["nix"].Enabled {
		t.Fatalf("features read back = %v", got)
	}

	for _, bad := range []string{"../brew", "core"} {
		_, err = create("bad", serverapi.SandboxCreateConfigFeatures{bad: {Enabled: false}})
		var status interface{ StatusCode() int }
		if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s: err = %v, want a 400", bad, err)
		}
	}

	manifest := model.SandboxManifest{HarnessMode: "run", Image: "img"}
	before := manifest.Fingerprint()
	manifest.Features = map[string]sandboxconfig.Feature{}
	if manifest.Fingerprint() != before {
		t.Fatal("an empty features map changed the fingerprint")
	}
}
