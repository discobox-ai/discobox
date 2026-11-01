package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func wantForbidden(t *testing.T, err error) {
	t.Helper()
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusForbidden {
		t.Fatalf("err = %v, want forbidden", err)
	}
}

// A person's box passes through every level's hosts; a box a discobox
// created inherits none, and has only its own level's (ADR 26-10-31-819 §2).
func TestPassthroughIsInheritedOnlyByAPersonsBox(t *testing.T) {
	ctx := context.Background()
	_, st := newUpgradeEngineFixture(t)
	person := egressFixture(t, st,
		egress.Policy{Passthrough: []string{"pinned.vendor.example"}},
		egress.Policy{Passthrough: []string{"*.corp.example"}},
		egress.Policy{},
		egress.Policy{Passthrough: []string{"own.example"}})
	got, err := st.EffectiveSandboxEgress(ctx, person)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"*.corp.example", "own.example", "pinned.vendor.example"}; !slices.Equal(got.Passthrough, want) {
		t.Fatalf("a person's box passes through %v, want %v", got.Passthrough, want)
	}

	child := *person
	creator := person.ID
	child.CreatedBySandboxID = &creator
	child.Egress = egress.Policy{}
	if got, err = st.EffectiveSandboxEgress(ctx, &child); err != nil {
		t.Fatal(err)
	}
	if len(got.Passthrough) != 0 {
		t.Fatalf("a discobox's box inherited passthrough %v", got.Passthrough)
	}
	child.Egress = egress.Policy{Passthrough: []string{"own.example"}}
	if got, err = st.EffectiveSandboxEgress(ctx, &child); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Passthrough, []string{"own.example"}) {
		t.Fatalf("a discobox's box passes through %v, want its own level's", got.Passthrough)
	}

	provision, err := st.ProvisionEgress(ctx, person)
	if err != nil {
		t.Fatal(err)
	}
	if len(provision.Passthrough) != 0 {
		t.Fatalf("provisioning passes through %v, want nothing", provision.Passthrough)
	}
}

func TestADiscoboxMayGiveANewOneOnlyPassthroughItHolds(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	creator := egressFixture(t, st, egress.Policy{}, egress.Policy{}, egress.Policy{}, egress.Policy{Passthrough: []string{"*.corp.example"}})
	child := func(own egress.Policy) *model.Sandbox {
		id := creator.ID
		return &model.Sandbox{ProjectID: "project-1", PoolID: "pool-1", SandboxManifest: creator.SandboxManifest, Egress: own, CreatedBySandboxID: &id}
	}
	if err := svc.refuseWiderThanCreator(ctx, creator.ID, child(egress.Policy{})); err != nil {
		t.Fatalf("no passthrough refused: %v", err)
	}
	if err := svc.refuseWiderThanCreator(ctx, creator.ID, child(egress.Policy{Passthrough: []string{"db.corp.example"}})); err != nil {
		t.Fatalf("a covered host refused: %v", err)
	}
	wantForbidden(t, svc.refuseWiderThanCreator(ctx, creator.ID, child(egress.Policy{Passthrough: []string{"other.example"}})))
}

func passthroughUpdate(level egress.Policy) services.UpdateSandboxBody {
	return services.UpdateSandboxBody{Config: serverapi.NewOptSandboxUpdateConfig(serverapi.SandboxUpdateConfig{
		Egress: serverapi.NewOptEgressPolicy(serverapi.EgressPolicy{
			Allow:         serverapi.NewOptNilStringArray(level.Allow),
			DenyByDefault: serverapi.NewOptBool(level.DenyByDefault),
			Passthrough:   serverapi.NewOptNilStringArray(level.Passthrough),
		}),
	})}
}

// A discobox may set a discobox's passthrough after create, within its own,
// and change nothing else (ADR 26-10-31-819 §5).
func TestADiscoboxMaySetPassthroughOnlyWithinItsOwn(t *testing.T) {
	ctx := context.Background()
	svc, st := newUpgradeEngineFixture(t)
	parent := egressFixture(t, st, egress.Policy{}, egress.Policy{}, egress.Policy{}, egress.Policy{Passthrough: []string{"foo.example"}})
	parentID := parent.ID
	child := eligibleSandbox(t, st, *parent.HarnessConfigID, codexBundle, codexBundleV1, func(sb *model.Sandbox) {
		sb.CreatedBySandboxID = &parentID
		sb.Egress = egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}}
	})
	asParent := auth.WithPrincipal(ctx, auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: parent.ID, ProjectID: "project-1"})

	updated, err := svc.UpdateSandbox(asParent, "project-1", child.ID,
		passthroughUpdate(egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}, Passthrough: []string{"foo.example"}}))
	if err != nil {
		t.Fatalf("a held host refused: %v", err)
	}
	if !slices.Equal(updated.Egress.Passthrough, []string{"foo.example"}) {
		t.Fatalf("stored passthrough = %v", updated.Egress.Passthrough)
	}
	if _, err := svc.UpdateSandbox(asParent, "project-1", child.ID,
		passthroughUpdate(egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}})); err != nil {
		t.Fatalf("narrowing back to none refused: %v", err)
	}

	for name, body := range map[string]services.UpdateSandboxBody{
		"a host the caller does not hold": passthroughUpdate(egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}, Passthrough: []string{"bar.example"}}),
		"a changed allow list":            passthroughUpdate(egress.Policy{DenyByDefault: true, Allow: []string{"github.com", "evil.example"}}),
		"lifting restriction":             passthroughUpdate(egress.Policy{Allow: []string{"github.com"}}),
		"a name": {Config: serverapi.NewOptSandboxUpdateConfig(serverapi.SandboxUpdateConfig{
			Name: serverapi.NewOptString("renamed"),
		})},
		"the discobox API flag beside passthrough": func() services.UpdateSandboxBody {
			body := passthroughUpdate(egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}, Passthrough: []string{"foo.example"}})
			body.DiscoboxApi = serverapi.NewOptBool(true)
			return body
		}(),
		"a harness beside passthrough": func() services.UpdateSandboxBody {
			body := passthroughUpdate(egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}, Passthrough: []string{"foo.example"}})
			config := body.Config.Value
			config.Harness = serverapi.NewOptString("codex")
			body.Config = serverapi.NewOptSandboxUpdateConfig(config)
			return body
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.UpdateSandbox(asParent, "project-1", child.ID, body)
			wantForbidden(t, err)
		})
	}

	// A person sets anything.
	if _, err := svc.UpdateSandbox(ctx, "project-1", child.ID,
		passthroughUpdate(egress.Policy{Passthrough: []string{"bar.example"}})); err != nil {
		t.Fatalf("a person's update refused: %v", err)
	}
}
