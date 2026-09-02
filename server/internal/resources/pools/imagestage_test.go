package pools

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// The set is what this server will actually run. Reading the compiled-in
// default instead meant a build pointed at released images staged a local tag,
// skipped it for being local, and left the largest image in the set unpulled.
func TestImageSetUsesTheResolvedSandboxImage(t *testing.T) {
	previous := defaultSandboxImage()
	t.Cleanup(func() { setDefaultSandboxImage(previous) })
	setDefaultSandboxImage("ghcr.io/discobox-ai/discobox-sandbox-agent:v1.2.3")

	images := stageableImages([]string{defaultSandboxImage(), "ghcr.io/x/harness-shell:v1"})
	if !slices.Contains(images, "ghcr.io/discobox-ai/discobox-sandbox-agent:v1.2.3") {
		t.Fatalf("images = %v, want the resolved sandbox image", images)
	}
}

// A local tag exists on no registry: pulling one fails on every development
// build, where the image is already on the daemon anyway.
func TestStageableImagesSkipsLocalTags(t *testing.T) {
	images := stageableImages([]string{"discobox-sandbox-agent:local", "discobox-harness-shell:local", "ghcr.io/x/codex:v1"})
	if len(images) != 1 || images[0] != "ghcr.io/x/codex:v1" {
		t.Fatalf("images = %v, want only the registry image", images)
	}
}

// Harness configs commonly share an image, and the order must not depend on
// map iteration or a status line says something different each pass.
func TestStageableImagesDedupesAndOrders(t *testing.T) {
	images := stageableImages([]string{"ghcr.io/x/b:v1", "ghcr.io/x/a:v1", "ghcr.io/x/b:v1", "  ", ""})
	if !slices.Equal(images, []string{"ghcr.io/x/a:v1", "ghcr.io/x/b:v1"}) {
		t.Fatalf("images = %v", images)
	}
}

// A failure has to say why, because staging retries quietly and the recorded
// condition is the only place a user could learn it is failing.
func TestImageStageCarriesItsFailure(t *testing.T) {
	stage := model.PoolImageStage{
		State: model.PoolImageStateFailed,
		Total: 4,
		Error: `pull image "ghcr.io/x/a:v1": unauthorized`,
	}
	if !strings.Contains(stage.Error, "unauthorized") {
		t.Fatalf("stage.Error = %q", stage.Error)
	}
	if stage.State != model.PoolImageStateFailed {
		t.Fatalf("state = %q", stage.State)
	}
}

// The progress a driver reports maps onto the condition without loss, so a
// client waiting out a first run can say what it is waiting for.
func TestImageStageCarriesPullProgress(t *testing.T) {
	progress := sandbox.PreloadProgress{
		Image: "ghcr.io/x/harness-claude-code:v1",
		Done:  1, Total: 4,
		Pull: &sandbox.PoolPullProgress{Current: 818 << 20, Total: 1400 << 20, Layers: 40, LayersComplete: 12},
	}
	stage := model.PoolImageStage{
		State: model.PoolImageStateStaging,
		Image: progress.Image, Done: progress.Done, Total: progress.Total,
		Current: progress.Pull.Current, Size: progress.Pull.Total,
		Layers: progress.Pull.Layers, LayersComplete: progress.Pull.LayersComplete,
	}
	if stage.Current != 818<<20 || stage.Size != 1400<<20 || stage.LayersComplete != 12 {
		t.Fatalf("stage = %+v, want the pull's counts", stage)
	}
}

// TestStagedPoolIsNotRemarkedEveryScan pins the mark as edge-triggered. Every
// pool re-reconciles on the 60s drift scan, so a mark on every successful pass
// re-staged every active pool once a minute forever: the staging resource's own
// six-hour refresh never got to fire, each pass re-inspected every image on the
// pool's daemon, and the log said "preloaded image" for each of them, forever.
func TestStagedPoolIsNotRemarkedEveryScan(t *testing.T) {
	ctx := context.Background()
	appStore, db := newPoolReconcilerTestStore(t)
	engine, err := reconcile.New(db, reconcile.Options{})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	manager := sandbox.NewProviderManager()
	manager.RegisterProvider("stub", stubPoolProvider{})

	provider := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "stub", Name: "stub"}
	if err := appStore.CreateSandboxProviderInstance(ctx, provider); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	registeredAt := time.Now().UTC()
	pool := &model.Pool{
		ID:           "pool-1",
		ProjectID:    "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: provider.ID},
		Ready:        true,
		Schedulable:  true,
		RegisteredAt: &registeredAt,
		LastSeenAt:   &registeredAt,
		ImagesStaged: true,
	}
	pool.DesiredState = model.DesiredStatePresent
	if err := appStore.CreatePool(ctx, pool); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	reconciler := NewPoolReconciler(appStore, manager, NewControlPlane(appStore, engine))
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dirty, err := engine.ListDirty(ctx, PoolImagesResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	if len(dirty) != 0 {
		t.Fatalf("dirty = %+v, want none: this pool's images are already staged", dirty)
	}

	// The other half: a pool that is not staged yet is still marked, so a host
	// that has just become usable does not wait out a scan interval.
	if err := db.WithContext(ctx).Model(&model.Pool{}).Where("id = ?", pool.ID).Update("images_staged", false).Error; err != nil {
		t.Fatalf("unstage pool: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, PoolDirtyID(pool.ProjectID, pool.ID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	dirty, err = engine.ListDirty(ctx, PoolImagesResourceType)
	if err != nil {
		t.Fatalf("list dirty: %v", err)
	}
	if len(dirty) != 1 || dirty[0].ResourceID != pool.ID {
		t.Fatalf("dirty = %+v, want the unstaged pool marked", dirty)
	}
}
