package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestPoolRegisterStatusAndSchedulableGate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	bootstrap := "bootstrap-token"
	h := sha256.Sum256([]byte(bootstrap))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create pool bootstrap: %v", err)
	}
	registered, err := s.RegisterPool(ctx, "pool-1", platform.Pool(), platform.OCI, h[:], "public", "ed25519")
	if err != nil {
		t.Fatalf("register pool: %v", err)
	}
	// Registration establishes identity and liveness only: health arrives on
	// the agent's own heartbeat, and State/ObservedGeneration are the
	// reconciler's to write.
	if registered.PublicKey != "public" || registered.RegisteredAt == nil || registered.LastSeenAt == nil {
		t.Fatalf("registered pool = %#v", registered)
	}
	if registered.Ready || registered.Schedulable {
		t.Fatalf("registration reported health: %#v", registered)
	}
	if registered.State == model.PoolStateActive {
		t.Fatal("registration wrote the reconciler's state")
	}
	updated, err := s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, true, 2, 4<<30, 10<<30, []byte(`{"pressure":"high"}`))
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !updated.Degraded || updated.AvailableCPUVCPUs != 2 || updated.AvailableMemoryBytes != 4<<30 || updated.AvailableStorageBytes != 10<<30 || string(updated.Conditions) == "" {
		t.Fatalf("updated pool = %#v", updated)
	}
	// A ready heartbeat cannot admit work while startup is still preloading.
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("startup admitted work: %v", err)
	}
	updated.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, updated, updated.Generation); err != nil {
		t.Fatal(err)
	}
	pool, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1"))
	if err != nil {
		t.Fatalf("schedulable pool: %v", err)
	}
	if pool.ID != "pool-1" {
		t.Fatalf("schedulable pool = %q, want pool-1", pool.ID)
	}
}

// TestUpdatePoolStatusLeavesReconcilerVerdictAlone pins the ownership split: a
// heartbeat reports health, and health alone. Writing State=active whenever
// the agent reported ready would repaint a recorded `offline` every few
// seconds — so a pool whose reconcile kept failing would read as active with a
// stale error hanging off it.
func TestUpdatePoolStatusLeavesReconcilerVerdictAlone(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	pool.RecordFailure(model.PoolStateOffline, "runtime did not converge", "")
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	updated, err := s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1<<30, 1<<30, nil)
	if err != nil {
		t.Fatalf("update status: %v", err)
	}
	if !updated.Ready || !updated.Schedulable || updated.LastSeenAt == nil {
		t.Fatalf("heartbeat did not record health: %#v", updated)
	}
	if updated.State != model.PoolStateOffline {
		t.Fatalf("state = %q, want the reconciler's %q to survive the heartbeat", updated.State, model.PoolStateOffline)
	}
	if updated.ErrorMessage == nil {
		t.Fatal("heartbeat dropped the recorded error; only a successful reconcile clears it")
	}
}

// TestSchedulablePoolForSandboxIgnoresCapacity pins that placement is never
// refused for low reported CPU/memory/storage (docs/adr/0029): sandboxes
// share their pool's resources with no per-sandbox reservation, so a pool
// reporting almost no available capacity is still schedulable as long as it
// is ready and schedulable.
func TestSchedulablePoolForSandboxIgnoresCapacity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 0, 0, 0, nil); err != nil {
		t.Fatalf("update status: %v", err)
	}

	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if pool, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); err != nil || pool.ID != "pool-1" {
		t.Fatalf("schedulable pool = %v err=%v, want pool-1", pool, err)
	}
}

func TestSchedulablePoolForSandboxRequiresReadiness(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	// A pool that never reported ready cannot accept placement.
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedulable pool error = %v, want ErrNotFound", err)
	}
}

// A fresh heartbeat recovers placement even if the runtime reconciler is still
// blocked and has not refreshed its older offline verdict.
func TestSchedulablePoolForSandboxUsesFreshHealthRatherThanLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.RecordFailure(model.PoolStateOffline, "pool agent has not reported", "")
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1<<30, 1<<30, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); err != nil {
		t.Fatalf("fresh heartbeat did not recover placement: %v", err)
	}
}

func TestPoolStartupInvalidatesHealthAndPreservesIdentity(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	token := sha256.Sum256([]byte("startup-health-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: pool.ID, TokenHash: token[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	pool, err = s.RegisterPool(ctx, pool.ID, platform.Pool(), platform.OCI, token[:], "durable-key", "ed25519")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	reported, err := s.UpdatePoolStatus(ctx, pool.ID, platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A restart follows the report it invalidates. Windows' clock can return
	// the same instant for both, which reads as a report from after the restart.
	for !time.Now().After(*reported.StatusReportedAt) {
		time.Sleep(time.Millisecond)
	}
	if err := s.BeginPoolHealthChecks(ctx); err != nil {
		t.Fatal(err)
	}
	pool, err = s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	if pool.Health(time.Now()) != model.PoolHealthUnknown || pool.PublicKey != "durable-key" || pool.RegisteredAt == nil || pool.State != model.PoolStateActive {
		t.Fatalf("restart changed identity/lifecycle or kept health: %+v", pool)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", pool.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("startup placement = %v, want wait", err)
	}
	if _, err := s.UpdatePoolStatus(ctx, pool.ID, platform.Pool(), platform.OCI, false, false, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	// A reconcile holding a pre-heartbeat snapshot must not erase freshness.
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", pool.ID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale reconcile overwrote a negative heartbeat: %v", err)
	}
	if _, err := s.UpdatePoolStatus(ctx, pool.ID, platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", pool.ID)); err != nil {
		t.Fatalf("fresh heartbeat did not reopen placement: %v", err)
	}
}

func TestPoolGenerationOptions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	pool, err := s.GetPoolByID(ctx, "pool-1", store.WithPoolGeneration(0))
	if err != nil {
		t.Fatalf("get matching generation: %v", err)
	}
	if _, err := s.GetPoolByID(ctx, "pool-1", store.WithPoolGeneration(pool.Generation+1)); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("get stale generation error = %v, want ErrGenerationConflict", err)
	}

	pool.Name = "pool-renamed"
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatalf("update matching generation: %v", err)
	}
	pool.Name = "pool-stale"
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation+1); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("update stale generation error = %v, want ErrGenerationConflict", err)
	}
}

func sandboxForClaim(projectID, poolID string) *model.Sandbox {
	return &model.Sandbox{
		ProjectID: projectID,
		PoolID:    poolID,
		Platform:  platform.Pool(),
	}
}

// TestPurgeSpentPoolBootstrapTokens pins the bound on the bootstrap token
// table: spent tokens (expired, used, or revoked) are collected, and a token
// that can still be redeemed is left alone.
func TestPurgeSpentPoolBootstrapTokens(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")

	live := sha256.Sum256([]byte("live-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: live[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create live bootstrap token: %v", err)
	}
	expired := sha256.Sum256([]byte("expired-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: expired[:], ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("create expired bootstrap token: %v", err)
	}

	purged, err := s.PurgeSpentPoolBootstrapTokens(ctx, time.Now())
	if err != nil {
		t.Fatalf("purge spent bootstrap tokens: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 spent token", purged)
	}

	// The live token still redeems: purging must not touch it.
	if _, err := s.RegisterPool(ctx, "pool-1", platform.Pool(), platform.OCI, live[:], "public", "ed25519"); err != nil {
		t.Fatalf("register pool with surviving live token: %v", err)
	}
}

func TestSchedulablePoolKeepsPreloadGateDespiteFreshHeartbeat(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateRegistering)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, sandboxForClaim("project-1", "pool-1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("placement during preload = %v, want ErrNotFound", err)
	}
}

// A pool hosts the one platform its agent declares, at registration and on
// every heartbeat, and placement refuses a sandbox of any other — as a
// mismatch naming both, not as a pool that is merely not ready yet, which a
// caller would wait on forever (ADR 0145 §1).
func TestSchedulablePoolForSandboxRefusesAnotherPlatform(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	h := sha256.Sum256([]byte("bootstrap-token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: h[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	arm := platform.Platform{OS: "linux", Arch: "arm64"}
	registered, err := s.RegisterPool(ctx, "pool-1", arm, platform.OCI, h[:], "public", "ed25519")
	if err != nil {
		t.Fatal(err)
	}
	if registered.Platform != arm {
		t.Fatalf("registered platform = %q, want %q", registered.Platform, arm)
	}
	// The heartbeat is what a pool from before platforms corrects itself by.
	amd := platform.Platform{OS: "linux", Arch: "amd64"}
	updated, err := s.UpdatePoolStatus(ctx, "pool-1", amd, platform.OCI, true, true, false, 1, 1<<30, 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Platform != amd {
		t.Fatalf("reported platform = %q, want %q", updated.Platform, amd)
	}
	updated.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, updated, updated.Generation); err != nil {
		t.Fatal(err)
	}

	sb := sandboxForClaim("project-1", "pool-1")
	sb.Platform = platform.Platform{OS: "darwin", Arch: "arm64"}
	_, err = s.SchedulablePoolForSandbox(ctx, sb)
	var mismatch *platform.MismatchError
	if !errors.As(err, &mismatch) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want a platform mismatch", err)
	}
	if mismatch.Sandbox != sb.Platform || mismatch.Pool != amd {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	sb.Platform = amd
	if _, err := s.SchedulablePoolForSandbox(ctx, sb); err != nil {
		t.Fatalf("a sandbox of the pool's own platform: %v", err)
	}
}

// An agent from before platforms declares none and still reports. Its pool
// keeps whatever was recorded for it, and nothing is guessed when nothing was:
// the pool may be any architecture. Placement on such a pool works as it did
// before platforms, settling no sandbox's platform.
func TestAnAgentThatDeclaresNoPlatformIsNotGuessedAt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	schedulablePool(t, s, "pool-1", platform.Platform{}, platform.OCI)
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	if !pool.Platform.IsZero() {
		t.Fatalf("platform = %q, want none guessed", pool.Platform)
	}
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", Name: "one", CreatedByUserID: "user-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1"}); err != nil {
		t.Fatalf("placement on an undeclared pool: %v", err)
	}
	if sb, err := s.GetSandbox(ctx, "project-1", "sbx-1"); err != nil || !sb.Platform.IsZero() {
		t.Fatalf("sandbox platform = %v, %v; want none written", sb.Platform, err)
	}

	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := s.UpdatePoolStatus(ctx, "pool-1", riscv, platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	pool, err = s.UpdatePoolStatus(ctx, "pool-1", platform.Platform{}, platform.OCI, true, true, false, 1, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Platform != riscv {
		t.Fatalf("platform = %q, want the recorded %q kept", pool.Platform, riscv)
	}
}

// schedulablePool brings a pool to where placement accepts it, hosting hosts
// and running images of kind runs.
func schedulablePool(t *testing.T, s *store.Store, poolID string, hosts platform.Platform, runs platform.ImageKind) {
	t.Helper()
	ctx := context.Background()
	createTestPool(t, s, "project-1", poolID)
	pool, err := s.UpdatePoolStatus(ctx, poolID, hosts, runs, true, true, false, 1, 1<<30, 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	if err := s.UpdatePoolWithGeneration(ctx, pool, pool.Generation); err != nil {
		t.Fatal(err)
	}
}

// A sandbox created while its pool's agent had declared nothing takes the
// pool's platform when it is placed, written to its row — provided its
// harness's image is published for it. A single-platform image on a pool of
// another platform is refused, saying what it is published for, and the row
// is left without a platform rather than given one it cannot run on.
func TestPlacementSettlesASandboxsPlatform(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	schedulablePool(t, s, "pool-1", riscv, platform.OCI)
	for slug, published := range map[string]platform.Set{
		"multi":  platform.NewSet(platform.Pool(), riscv),
		"single": platform.NewSet(platform.Pool()),
	} {
		if err := s.CreateHarnessConfig(ctx, &model.HarnessConfig{ID: "hc-" + slug, ProjectID: "project-1", Slug: slug, Name: slug, Platforms: published}); err != nil {
			t.Fatal(err)
		}
		harnessID := "hc-" + slug
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-" + slug, ProjectID: "project-1", PoolID: "pool-1", Name: slug, CreatedByUserID: "user-1",
			SandboxManifest: model.SandboxManifest{HarnessConfigID: &harnessID}}); err != nil {
			t.Fatal(err)
		}
	}
	// The stand-in a provider places with carries no platform: the row was
	// written before the pool declared one.
	standIn := func(id string) *model.Sandbox {
		return &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: "pool-1"}
	}

	if _, err := s.SchedulablePoolForSandbox(ctx, standIn("sbx-multi")); err != nil {
		t.Fatalf("a multi-platform harness on a pool of one of its platforms: %v", err)
	}
	if sb, err := s.GetSandbox(ctx, "project-1", "sbx-multi"); err != nil || sb.Platform != riscv {
		t.Fatalf("platform = %v, %v; want the pool's %q recorded", sb.Platform, err, riscv)
	}

	_, err := s.SchedulablePoolForSandbox(ctx, standIn("sbx-single"))
	var unpublished *platform.UnpublishedError
	if !errors.As(err, &unpublished) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want the image's platforms as the refusal", err)
	}
	if sb, err := s.GetSandbox(ctx, "project-1", "sbx-single"); err != nil || !sb.Platform.IsZero() {
		t.Fatalf("platform = %v, %v; want none recorded", sb.Platform, err)
	}
}

// An import places its tree before the row exists, with a stand-in that
// carries the archive's platform; one from before platforms carries none and
// lands on its pool rather than being refused as no platform at all.
func TestPlacementOfAStandInFromBeforePlatforms(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	schedulablePool(t, s, "pool-1", riscv, platform.OCI)
	if _, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: "sbx-new", ProjectID: "project-1", PoolID: "pool-1"}); err != nil {
		t.Fatalf("a stand-in with no platform: %v", err)
	}
	_, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: "sbx-new", ProjectID: "project-1", PoolID: "pool-1", Platform: platform.Pool()})
	var mismatch *platform.MismatchError
	if riscv != platform.Pool() && !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a stand-in of another platform refused", err)
	}
}

// Placement writes the platform it settles onto the row, and the reconciler
// then saves the sandbox it loaded before placing it — whose platform is
// still empty. That save must not undo the settlement.
func TestASettledPlatformSurvivesTheReconcilersSave(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	schedulablePool(t, s, "pool-1", riscv, platform.OCI)
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1", Name: "one", CreatedByUserID: "user-1"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.GetSandbox(ctx, "project-1", "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: "sbx-1", ProjectID: "project-1", PoolID: "pool-1"}); err != nil {
		t.Fatal(err)
	}
	for _, update := range []func() error{
		func() error { return s.UpdateSandbox(ctx, loaded) },
		func() error { return s.UpdateSandbox(ctx, loaded, store.WithGeneration(loaded.Generation)) },
	} {
		if err := update(); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSandbox(ctx, "project-1", "sbx-1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Platform != riscv {
			t.Fatalf("platform = %q after the reconciler's save, want the settled %q", got.Platform, riscv)
		}
	}
}

// A pool that has declared no platform places a sandbox that has none, as
// before platforms, but refuses one that has: only an import's tree brings a
// platform onto such a pool, and nothing could check it there.
func TestAnUndeclaredPoolRefusesASandboxWithAPlatform(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	schedulablePool(t, s, "pool-1", platform.Platform{}, platform.OCI)
	_, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: "sbx-import", ProjectID: "project-1", PoolID: "pool-1", Platform: platform.Pool()})
	var mismatch *platform.MismatchError
	if !errors.As(err, &mismatch) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want a mismatch for a stand-in with a platform", err)
	}
	if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sbx-row", ProjectID: "project-1", PoolID: "pool-1", Name: "row", CreatedByUserID: "user-1", Platform: platform.Pool()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: "sbx-row", ProjectID: "project-1", PoolID: "pool-1"}); !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want a mismatch for a row with a platform", err)
	}
}

// A pool runs one kind of image, and a sandbox is placed only where its
// harness's image is that kind as well as the pool's platform (ADR 26-10-09-106
// §4). A boxd pool and a Docker pool both host linux on this architecture: a
// disco-vm harness for boxd is refused on the Docker pool with the kinds as the
// reason — never ErrNotFound, which a caller would wait on for good — and
// placed on the boxd pool, and an OCI harness the other way round.
func TestPlacementMatchesTheImageKind(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	boxd := platform.DiscoVM("boxd")
	schedulablePool(t, s, "docker", platform.Pool(), platform.OCI)
	schedulablePool(t, s, "boxd", platform.Pool(), boxd)
	for slug, kind := range map[string]platform.ImageKind{"machine": boxd, "container": platform.OCI, "undeclared": {}} {
		if err := s.CreateHarnessConfig(ctx, &model.HarnessConfig{ID: "hc-" + slug, ProjectID: "project-1", Slug: slug, Name: slug,
			Platforms: platform.NewSet(platform.Pool()), ImageKind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	// The column's default stands in for a kind left out of an insert, so the
	// undeclared one — what the migration leaves a manifest-file harness — is
	// written the way the migration writes it.
	if err := s.UpdateHarnessConfig(ctx, &model.HarnessConfig{ID: "hc-undeclared", ProjectID: "project-1", Slug: "undeclared", Name: "undeclared",
		Platforms: platform.NewSet(platform.Pool())}); err != nil {
		t.Fatal(err)
	}
	place := func(harness, pool string) error {
		harnessID := "hc-" + harness
		id := "sbx-" + harness + "-" + pool
		if err := s.CreateSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: pool, Name: id, CreatedByUserID: "user-1",
			SandboxManifest: model.SandboxManifest{HarnessConfigID: &harnessID}}); err != nil {
			t.Fatal(err)
		}
		_, err := s.SchedulablePoolForSandbox(ctx, &model.Sandbox{ID: id, ProjectID: "project-1", PoolID: pool})
		return err
	}

	err := place("machine", "docker")
	var mismatch *platform.KindMismatchError
	if !errors.As(err, &mismatch) || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a disco-vm harness on a Docker pool of its platform: err = %v, want a kind mismatch", err)
	}
	if mismatch.Image != boxd || mismatch.Pool != platform.OCI {
		t.Fatalf("mismatch = %+v", mismatch)
	}
	if !strings.Contains(err.Error(), "disco-vm image for the boxd driver, and the pool runs OCI images") {
		t.Fatalf("err = %q, want it to say both kinds", err)
	}
	if err := place("machine", "boxd"); err != nil {
		t.Fatalf("a disco-vm harness on a pool of its driver: %v", err)
	}
	if err := place("container", "boxd"); !errors.As(err, &mismatch) {
		t.Fatalf("an OCI harness on a boxd pool: err = %v, want a kind mismatch", err)
	}
	if err := place("container", "docker"); err != nil {
		t.Fatalf("an OCI harness on a Docker pool: %v", err)
	}
	for _, pool := range []string{"docker", "boxd"} {
		if err := place("undeclared", pool); !errors.As(err, &mismatch) {
			t.Fatalf("a harness of no declared kind on %s: err = %v, want a kind mismatch", pool, err)
		}
	}
}

// An agent from before image kinds declares none, and runs OCI images — there
// was no other kind — so OCI is what its pool records, at registration and on
// every report. An agent that declares a kind has it recorded as declared.
func TestAnAgentThatDeclaresNoImageKindRunsOCI(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	token := sha256.Sum256([]byte("token"))
	if err := s.CreatePoolBootstrapToken(ctx, &model.PoolBootstrapToken{PoolID: "pool-1", TokenHash: token[:], ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	pool, err := s.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	if !pool.ImageKind.IsZero() {
		t.Fatalf("image kind = %q before the agent registered, want none", pool.ImageKind)
	}
	pool, err = s.RegisterPool(ctx, "pool-1", platform.Pool(), platform.ImageKind{}, token[:], "public", "ed25519")
	if err != nil {
		t.Fatal(err)
	}
	if pool.ImageKind != platform.OCI {
		t.Fatalf("registered with no kind: image kind = %q, want oci", pool.ImageKind)
	}
	pool, err = s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.DiscoVM("boxd"), true, true, false, 1, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pool.ImageKind != platform.DiscoVM("boxd") {
		t.Fatalf("reported discovm/boxd: image kind = %q", pool.ImageKind)
	}
	pool, err = s.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.ImageKind{}, true, true, false, 1, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pool.ImageKind != platform.OCI {
		t.Fatalf("reported no kind: image kind = %q, want oci", pool.ImageKind)
	}
}
