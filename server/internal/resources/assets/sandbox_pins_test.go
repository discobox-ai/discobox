package assets

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// A sandbox keeps its pin after its harness config moves on (ADR 26-10-16-423
// §7), so its pool is still answered the bundle the box was handed, and the
// store keeps it. Another project's pool is not.
func TestASandboxsPinStaysInScopeAfterItsConfigMoves(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.db.Write.WithContext(ctx).Create(&model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", CreatedByUserID: "user-1", PoolID: "pool-1", Name: "one",
		SandboxManifest: model.SandboxManifest{Bundle: acmeRef, BundleDigest: f.acme.manifest},
	}).Error; err != nil {
		t.Fatal(err)
	}
	moved := f.registry.image(t, "acme v2")
	f.registry.tags[acmeRef] = moved.manifest
	if err := f.service.store.UpdateHarnessConfig(ctx, &model.HarnessConfig{ID: "hc-1", ProjectID: "project-1", Slug: "acme", Name: "Acme", Bundle: acmeRef, BundleDigest: moved.manifest}); err != nil {
		t.Fatal(err)
	}

	content, err := f.service.AssetManifest(asPool("pool-1"), "pool-1", "discobox-harness-acme", f.acme.manifest)
	if err != nil {
		t.Fatalf("the sandbox's pinned manifest after its config moved: %v", err)
	}
	read(t, content)
	read(t, mustBlob(t, f, f.acme.layer))
	// The name answers the config's pin, not the sandbox's: a sandbox pin is
	// digest scope only.
	if content, err := f.service.ResolveAssetName(asPool("pool-1"), "pool-1", "discobox-harness-acme"); err != nil {
		t.Fatal(err)
	} else if content.Digest != moved.manifest {
		t.Fatalf("name resolved to %s, want the config's %s", content.Digest, moved.manifest)
	} else {
		read(t, content)
	}
	_, err = f.service.AssetManifest(asPool("pool-2"), "pool-2", "discobox-harness-acme", f.acme.manifest)
	wantStatus(t, err, 404)

	f.service.now = func() time.Time { return time.Now().Add(pruneGrace + platformRetry) }
	if _, err := f.service.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	for _, digest := range []string{f.acme.manifest, f.acme.layer} {
		if !f.service.has(digest) {
			t.Fatalf("%s the sandbox pins was pruned", digest)
		}
	}
}

// The sandboxes are told each time the release's platform reference resolves
// somewhere new, the first time included, and not when it resolves where it
// was; they are handed what it resolved to.
func TestThePlatformMovingIsTold(t *testing.T) {
	f := newFixture(t)
	moves := make(chan struct{}, 4)
	f.service.OnPlatformMoved(func(context.Context) { moves <- struct{}{} })
	wantMoves := func(n int) {
		t.Helper()
		for range n {
			select {
			case <-moves:
			case <-time.After(5 * time.Second):
				t.Fatal("the platform moving was not told")
			}
		}
		select {
		case <-moves:
			t.Fatal("told of a move that was none")
		case <-time.After(50 * time.Millisecond):
		}
	}

	if pin := f.service.LatestPlatform(); pin.Digest != "" {
		t.Fatalf("latest before any resolution = %+v, want none", pin)
	}
	f.service.ResolvePlatform(context.Background())
	wantMoves(1)
	pin, err := f.service.CurrentPlatform(context.Background())
	if err != nil || pin.Reference != platformRef || pin.Digest != f.index {
		t.Fatalf("current platform = %+v, %v; want %s@%s", pin, err, platformRef, f.index)
	}
	if latest := f.service.LatestPlatform(); latest != pin {
		t.Fatalf("latest = %+v, want %+v", latest, pin)
	}
	f.service.ResolvePlatform(context.Background())
	wantMoves(0)

	moved := f.registry.index(t, f.registry.image(t, "platform v2"))
	f.registry.tags[platformRef] = moved
	f.service.now = func() time.Time { return time.Now().Add(platformRetry) }
	if pin, err := f.service.CurrentPlatform(context.Background()); err != nil || pin.Digest != moved {
		t.Fatalf("current platform after the tag moved = %+v, %v; want %s", pin, err, moved)
	}
	wantMoves(1)
}
