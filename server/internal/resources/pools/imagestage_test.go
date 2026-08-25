package pools

import (
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
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
