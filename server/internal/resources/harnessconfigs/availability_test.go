package harnessconfigs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

type unavailableInspector struct{ err error }

func (i unavailableInspector) Inspect(context.Context, string) (imageMetadata, error) {
	return imageMetadata{}, i.err
}

// Seeding skips an image it cannot inspect, so one broken harness cannot stop a
// server. Skipping every one of them is a different condition: the project can
// create no sandbox at all (ADR 0048), which is what startup refuses to serve.
//
// The error names each harness and why its image was unusable, because "no
// harness" on its own describes none of the causes — an image published without
// its manifest labels, a daemon that is not running, a registry out of reach —
// and they are the only actionable part.
func TestEnsureHarnessAvailableReportsEveryUnusableImage(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	svc := &Service{store: st, inspector: unavailableInspector{
		err: errors.New(`image label "10-sandbox-base" is empty`),
	}}

	// Seeding still succeeds: it is best-effort per harness.
	if err := svc.SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	configs, err := st.ListHarnessConfigs(ctx, "project-1")
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	if len(configs) != 0 {
		t.Fatalf("configs = %v, want none seeded from unusable images", configs)
	}

	err = svc.EnsureHarnessAvailable(ctx, "project-1")
	if err == nil {
		t.Fatal("EnsureHarnessAvailable = nil, want a project with no harness to be refused")
	}
	seeds := svc.seeds()
	if len(seeds) == 0 {
		t.Fatal("no built-in harnesses are defined; this test proves nothing")
	}
	for _, seed := range seeds {
		if !strings.Contains(err.Error(), seed.Slug) {
			t.Fatalf("error = %q, want it to name the %s harness", err, seed.Slug)
		}
	}
	if !strings.Contains(err.Error(), "10-sandbox-base") {
		t.Fatalf("error = %q, want the reason each image was unusable", err)
	}
}

// Any config counts, not only a seeded built-in. A project that seeded on an
// earlier boot, or whose only harness was registered by hand, has something to
// run whatever this boot's images do — so a registry outage cannot stop a server
// that was working yesterday.
func TestEnsureHarnessAvailableAcceptsAConfigItDidNotSeed(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.CreateHarnessConfig(ctx, &model.HarnessConfig{
		ProjectID: "project-1", Slug: "mine", Name: "Mine",
		Image: "example.com/mine:v1", ImageDigest: "sha256:mine", Configured: true,
	}); err != nil {
		t.Fatalf("create config: %v", err)
	}
	svc := &Service{store: st, inspector: unavailableInspector{err: errors.New("no such image")}}

	if err := svc.EnsureHarnessAvailable(ctx, "project-1"); err != nil {
		t.Fatalf("EnsureHarnessAvailable = %v, want nil when the project already has a harness", err)
	}
}
