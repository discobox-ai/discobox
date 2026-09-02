package harnessconfigs

import (
	"context"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// A custom harness has no trigger but this one (ADR 0016 §7), so re-pulling its
// image is the whole of how its sandboxes ever move (ADR 0082 §1).
func TestRefreshingACustomHarnessImageUpgradesItsSandboxes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: "custom", Name: "Custom",
		Image: "registry.example/custom:v1", ImageDigest: "sha256:one",
	}
	if err := st.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	runtime := &stubSandboxRuntime{}
	inspector := &stubInspector{byImage: map[string]imageMetadata{
		"registry.example/custom:v1": {Digest: "sha256:two"},
	}}
	svc := &Service{store: st, inspector: inspector, sandboxes: runtime}

	if _, err := svc.RefreshHarnessConfigImage(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("refresh harness config image: %v", err)
	}
	if len(runtime.upgraded) != 1 || runtime.upgraded[0] != config.ID {
		t.Fatalf("upgraded = %v, want one fan-out for %s", runtime.upgraded, config.ID)
	}

	// The tag now resolves to what the config already records. Nothing moved,
	// so nothing is upgraded: the fan-out costs a query per config and this
	// runs on every refresh.
	if _, err := svc.RefreshHarnessConfigImage(ctx, "project-1", config.ID); err != nil {
		t.Fatalf("refresh harness config image again: %v", err)
	}
	if len(runtime.upgraded) != 1 {
		t.Fatalf("upgraded = %v, want no second fan-out for an unchanged digest", runtime.upgraded)
	}
}

// The dev loop: a stable tag rebuilt in place, reseeded on the next project
// ensure. Creating a config upgrades nothing — nothing references it yet — and
// the rebuild that follows is what carries its stopped sandboxes forward.
func TestReseedingABuiltInImageUpgradesItsSandboxes(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	const image = "discobox-harness-stub:local"

	overrides := map[string]string{}
	for _, seed := range harnessdefs.Seeds(nil) {
		overrides[seed.Slug] = image
	}
	if len(overrides) == 0 {
		t.Fatal("no built-in harnesses to seed")
	}
	runtime := &stubSandboxRuntime{}
	inspector := &stubInspector{byImage: map[string]imageMetadata{image: {Digest: "sha256:one"}}}
	svc := &Service{store: st, inspector: inspector, harnessImages: overrides, sandboxes: runtime}

	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed built-ins: %v", err)
	}
	if len(runtime.upgraded) != 0 {
		t.Fatalf("upgraded = %v, want none: a config being created has no sandboxes", runtime.upgraded)
	}

	// Same reference, rebuilt: the case tag comparison misses and digest
	// comparison catches (ADR 0016 §7).
	inspector.byImage[image] = imageMetadata{Digest: "sha256:two"}
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("reseed built-ins: %v", err)
	}
	if len(runtime.upgraded) != len(overrides) {
		t.Fatalf("upgraded = %d configs, want all %d whose digest moved", len(runtime.upgraded), len(overrides))
	}

	// And a pass that resolves the same digest again is inert.
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("reseed built-ins again: %v", err)
	}
	if len(runtime.upgraded) != len(overrides) {
		t.Fatalf("upgraded = %d, want no further fan-out for an unchanged digest", len(runtime.upgraded))
	}
}
