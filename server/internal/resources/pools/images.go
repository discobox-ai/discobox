package pools

import (
	"context"
	"sort"
	"strings"
	"sync"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// imageSet is every image a sandbox on this project might run: the default
// sandbox image this server resolved, and the image of every harness config the
// project has.
//
// Read from the project's harness configs rather than from the built-in list,
// because a project can register its own and those are exactly as much of a
// first-run wait as the built-in three.
func (r *PoolReconciler) imageSet(ctx context.Context, projectID string) ([]string, error) {
	configs, err := r.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	images := make([]string, 0, len(configs)+1)
	images = append(images, defaultSandboxImage())
	for i := range configs {
		images = append(images, configs[i].Image)
	}
	return stageableImages(images), nil
}

// stageableImages deduplicates and orders release images. Development local
// tags are delivered by the development image sync before the agent starts.
func stageableImages(images []string) []string {
	seen := map[string]struct{}{}
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" || strings.HasSuffix(image, ":local") {
			continue
		}
		seen[image] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for image := range seen {
		out = append(out, image)
	}
	// Stable order, so what a status line says does not depend on map order.
	sort.Strings(out)
	return out
}

// The image a sandbox with no harness config runs, as this server resolved it.
//
// Package-level and guarded, rather than a field: the reconciler is built
// during app construction and the value is resolved from configuration a moment
// later, and there is exactly one answer per process.
var (
	defaultSandboxImageMu sync.RWMutex
	resolvedSandboxImage  string
)

func setDefaultSandboxImage(image string) {
	defaultSandboxImageMu.Lock()
	defer defaultSandboxImageMu.Unlock()
	resolvedSandboxImage = strings.TrimSpace(image)
}

func defaultSandboxImage() string {
	defaultSandboxImageMu.RLock()
	defer defaultSandboxImageMu.RUnlock()
	if resolvedSandboxImage != "" {
		return resolvedSandboxImage
	}
	return sandbox.DefaultSandboxImageName
}
