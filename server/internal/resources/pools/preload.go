package pools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// Preloading the images a sandbox will want, at server startup, onto every pool
// that already exists.
//
// The alternative is what it replaces: the first sandbox on a cold machine
// waits for a VM to boot and for two gigabytes of harness image to arrive, and
// every later one waits for whatever it is the first to ask for. Doing it once
// at startup makes that a single wait, in a place where the server can say what
// it is doing, instead of an unpredictable one attached to whichever command
// the user happened to run first.

// PreloadImages brings every known pool up and pulls the images a sandbox will
// want onto it.
//
// Errors are collected, never fatal. A pool that cannot come up, or an image
// that no longer exists, must not stop a server from starting: preloading is an
// optimisation for a wait that would otherwise happen later, and a server that
// refuses to start because of one is strictly worse than the wait.
func (s *ControlPlane) PreloadImages(ctx context.Context, report func(line string)) error {
	if s.providerManager == nil {
		return errors.New("provider manager is required")
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for i := range projects {
		project := &projects[i]
		pools, err := s.store.ListPools(ctx, project.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		images, err := s.preloadImageSet(ctx, project.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for j := range pools {
			pool := &pools[j]
			if pool.RevokedAt != nil || pool.DesiredState != model.DesiredStatePresent {
				continue
			}
			if err := s.preloadPool(ctx, project, pool, images, report); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.WarnContext(ctx, "preload pool failed", "pool", pool.ID, "error", err)
				failures = append(failures, fmt.Errorf("pool %s: %w", pool.ID, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (s *ControlPlane) preloadPool(ctx context.Context, project *model.Project, pool *model.Pool, images []string, report func(line string)) error {
	instance, err := s.store.GetSandboxProviderInstance(ctx, project.ID, pool.ProviderInstanceID)
	if err != nil {
		return err
	}
	if instance.Disabled {
		return nil
	}
	provider, err := s.providerManager.ResolveInstance(ctx, instance)
	if err != nil {
		return err
	}
	runtime, ok := provider.(sandbox.PoolRuntime)
	if !ok {
		// A provider with no pool runtime has no daemon to preload onto.
		return nil
	}
	name := pool.Name
	if strings.TrimSpace(name) == "" {
		name = pool.ID
	}
	return runtime.PreloadImages(ctx, s, project, instance, pool, images, func(image string, done, total int) {
		if report == nil {
			return
		}
		if image == "" {
			report(fmt.Sprintf("%s: %d of %d images ready", name, done, total))
			return
		}
		report(fmt.Sprintf("%s: %s (%d of %d)", name, imageLabel(image), done+1, total))
	})
}

// preloadImageSet is every image a sandbox on this project might run: the
// default sandbox image, and the image of every harness config the project has.
//
// Read from the project's harness configs rather than from the built-in list,
// because a project can register its own and those are exactly as much a
// first-run wait as the built-in three.
func (s *ControlPlane) preloadImageSet(ctx context.Context, projectID string) ([]string, error) {
	seen := map[string]struct{}{}
	add := func(image string) {
		image = strings.TrimSpace(image)
		// A local tag exists on no registry; pulling one fails on every
		// development build, where the image is already there anyway.
		if image == "" || strings.HasSuffix(image, ":local") {
			return
		}
		seen[image] = struct{}{}
	}
	add(sandbox.DefaultSandboxImageName)
	configs, err := s.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range configs {
		add(configs[i].Image)
	}
	images := make([]string, 0, len(seen))
	for image := range seen {
		images = append(images, image)
	}
	// Stable order so a report reads the same way twice.
	sort.Strings(images)
	return images, nil
}

// imageLabel shortens an image reference to the part that identifies it, the
// way the CLI does: a status line has one line, and the registry host and
// namespace are the same for every image here.
func imageLabel(image string) string {
	label := image
	if slash := strings.LastIndex(label, "/"); slash >= 0 {
		label = label[slash+1:]
	}
	return label
}
