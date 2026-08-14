// Package imagereap reclaims disk on a Docker daemon by removing Discobox
// images that nothing uses any more (ADR 0039).
//
// Two things make an image reclaimable, and both are required. The image must
// carry harness.ReclaimLabel, which is the boundary of what Discobox is entitled
// to delete on a daemon it shares with a developer's own images; and it must
// have arrived on this daemon longer ago than the retention window without any
// container, running or stopped, referring to it.
//
// "Arrived" is the daemon's own LastTagTime, not the image's Created timestamp.
// Created is when whoever published the image built it, so a release image built
// weeks before it shipped would be stale the instant it was pulled. LastTagTime
// is stamped on build, pull (including a re-pull that only re-applies an
// unchanged tag), load, and tag — every way an image gets here.
//
// The package is split the way the volume reaper is: Reclaimable decides from
// plain data and is where the rules are tested, while Reclaim does the daemon
// I/O around it.
package imagereap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/harness"
)

const (
	// RetentionEnv overrides how long an unused Discobox image is kept after it
	// last arrived on the daemon. It is read by the server for the host daemon
	// and by the pool agent for its own, and the server propagates a configured
	// value into the pool container so one setting governs both.
	RetentionEnv = "DISCOBOX_IMAGE_RETENTION"

	// DefaultRetention matches sandboxVolumeRetention, the window a dead
	// sandbox's volume tree already gets, for the same reason: long enough that
	// an accidental removal and a same-day recreate cost nothing.
	DefaultRetention = 24 * time.Hour

	// DevelopmentRetention is the default while the image watcher is driving the
	// daemon. A rebuild loop supersedes a multi-gigabyte image every few
	// minutes, so a day's grace is a day of images: the window has to be shorter
	// than the loop that produces them or it reclaims nothing that matters.
	//
	// It can be this short safely because superseded is unambiguous here.
	// Anything still wanted is either named by a container or named by the
	// current development manifest, and both are checked before age is.
	DevelopmentRetention = 15 * time.Minute

	// minReclaimInterval and maxReclaimInterval bound how often a pass runs.
	minReclaimInterval = time.Minute
	maxReclaimInterval = time.Hour
)

// ReclaimInterval is how often to run a pass for a given retention window.
//
// It is derived rather than configured because the two are useless apart: a
// 15-minute window checked hourly reclaims on the hour regardless, and an hourly
// window checked every minute is 59 wasted passes. Half the window bounds the
// lag at 1.5x the retention, and the clamps keep a pathological setting from
// turning into a busy loop or an effectively dead one.
//
// This is also what carries the development cadence into a pool, which has no
// other way to know: the pool agent is handed a retention, not a mode.
func ReclaimInterval(retention time.Duration) time.Duration {
	interval := retention / 2
	if interval < minReclaimInterval {
		return minReclaimInterval
	}
	if interval > maxReclaimInterval {
		return maxReclaimInterval
	}
	return interval
}

// RetentionFromEnv resolves the retention window from RetentionEnv, falling back
// to DefaultRetention when unset. A non-positive or unparsable value is an
// error rather than a silent fallback: the two ways to get this wrong are
// deleting images that are still wanted and never reclaiming anything, and both
// should be loud.
func RetentionFromEnv() (time.Duration, error) {
	retention, err := ConfiguredRetention()
	if err != nil {
		return 0, err
	}
	if retention == 0 {
		return DefaultRetention, nil
	}
	return retention, nil
}

// ConfiguredRetention is RetentionFromEnv without the default applied: it
// returns zero when RetentionEnv is unset. It exists for the one caller that
// must tell "not configured" apart from "configured to the default value" — the
// engine records the override in the pool container's configuration, and
// materializing a default there would change that configuration's revision and
// recreate every existing pool for no reason.
func ConfiguredRetention() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(RetentionEnv))
	if value == "" {
		return 0, nil
	}
	retention, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", RetentionEnv, err)
	}
	if retention <= 0 {
		return 0, fmt.Errorf("%s must be greater than 0, got %s", RetentionEnv, value)
	}
	return retention, nil
}

// Candidate is one labeled image as the daemon reports it.
type Candidate struct {
	ID          string
	RepoTags    []string
	RepoDigests []string
	// LastLocal is the daemon's LastTagTime: when this image last arrived here.
	// Zero means the daemon did not report one, which is treated as an unknown
	// age and never reclaimed.
	LastLocal time.Time
}

// references returns every way this image can be named, so a keep entry matches
// whether it was written as an ID, a tag, or a digest reference.
func (c Candidate) references() []string {
	refs := make([]string, 0, len(c.RepoTags)+len(c.RepoDigests)+1)
	refs = append(refs, c.ID)
	refs = append(refs, c.RepoTags...)
	refs = append(refs, c.RepoDigests...)
	return refs
}

// Reclaimable returns the candidates that may be removed.
//
// inUse holds the image IDs containers refer to, keep holds references and IDs
// that must survive regardless of age, and now is the clock. An image is
// reclaimable only when it is in neither set and its local age exceeds
// retention.
func Reclaimable(candidates []Candidate, inUse, keep map[string]struct{}, retention time.Duration, now time.Time) []Candidate {
	reclaimable := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" {
			continue
		}
		if _, used := inUse[candidate.ID]; used {
			continue
		}
		if keepsAny(keep, candidate.references()) {
			continue
		}
		// No knowable local age. Reclaiming on a guess here would mean deleting
		// an image the daemon simply declined to describe.
		if candidate.LastLocal.IsZero() {
			continue
		}
		if now.Sub(candidate.LastLocal) < retention {
			continue
		}
		reclaimable = append(reclaimable, candidate)
	}
	return reclaimable
}

func keepsAny(keep map[string]struct{}, references []string) bool {
	for _, reference := range references {
		if reference == "" {
			continue
		}
		if _, ok := keep[reference]; ok {
			return true
		}
	}
	return false
}

// Options configures one reclamation pass.
type Options struct {
	// Retention is how long an unused image is kept after it last arrived.
	Retention time.Duration
	// Keep names images that must survive whatever their age, by ID or by any
	// reference. It covers what is needed but not currently running: the
	// configured pool image, and a development base image that only other images
	// are built FROM.
	Keep []string
	// Now defaults to time.Now.
	Now time.Time
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Result reports what one pass did.
type Result struct {
	// Scanned is how many labeled images the daemon reported.
	Scanned int
	// Removed lists the image IDs actually reclaimed.
	Removed []string
	// Retained is how many labeled images were kept, for any reason.
	Retained int
}

// Reclaim removes the labeled images on cli that nothing uses and that have
// outlived the retention window.
//
// A removal failure is not a pass failure. The common one is an image other
// images were built FROM, which the classic graph driver refuses to delete while
// a child exists; that resolves itself once the child goes, so the pass logs it
// and carries on rather than abandoning the images it could still reclaim.
func Reclaim(ctx context.Context, cli *client.Client, opts Options) (Result, error) {
	if cli == nil {
		return Result{}, errors.New("docker client is required")
	}
	retention := opts.Retention
	if retention <= 0 {
		retention = DefaultRetention
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	inUse, err := imagesInUse(ctx, cli)
	if err != nil {
		return Result{}, err
	}
	candidates, err := labeledImages(ctx, cli)
	if err != nil {
		return Result{}, err
	}
	keep := make(map[string]struct{}, len(opts.Keep))
	for _, reference := range opts.Keep {
		if reference = strings.TrimSpace(reference); reference != "" {
			keep[reference] = struct{}{}
		}
	}

	result := Result{Scanned: len(candidates)}
	for _, candidate := range Reclaimable(candidates, inUse, keep, retention, now) {
		if err := remove(ctx, cli, candidate); err != nil {
			logger.DebugContext(ctx, "retaining Discobox image the daemon declined to remove",
				"image", candidate.ID, "tags", candidate.RepoTags, "error", err)
			continue
		}
		logger.InfoContext(ctx, "reclaimed unused Discobox image",
			"image", candidate.ID, "tags", candidate.RepoTags,
			"age", now.Sub(candidate.LastLocal).Truncate(time.Minute))
		result.Removed = append(result.Removed, candidate.ID)
	}
	result.Retained = result.Scanned - len(result.Removed)
	return result, nil
}

// imagesInUse collects the image IDs every container on the daemon refers to.
//
// Stopped containers count. A stopped sandbox keeps its container and must be
// able to start again from the image it was built from, so power state says
// nothing about whether an image is still needed.
func imagesInUse(ctx context.Context, cli *client.Client) (map[string]struct{}, error) {
	containers, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	inUse := make(map[string]struct{}, len(containers.Items))
	for _, ctr := range containers.Items {
		if id := strings.TrimSpace(ctr.ImageID); id != "" {
			inUse[id] = struct{}{}
		}
	}
	return inUse, nil
}

func labeledImages(ctx context.Context, cli *client.Client) ([]Candidate, error) {
	filters := client.Filters{}
	filters = filters.Add("label", harness.ReclaimLabel+"="+harness.ReclaimLabelValue)
	images, err := cli.ImageList(ctx, client.ImageListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list Discobox images: %w", err)
	}
	candidates := make([]Candidate, 0, len(images.Items))
	for _, image := range images.Items {
		// The local arrival time is engine-local metadata, so it is only on the
		// inspect response and costs one call per labeled image.
		inspect, err := cli.ImageInspect(ctx, image.ID)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("inspect image %s: %w", image.ID, err)
		}
		candidates = append(candidates, Candidate{
			ID:          image.ID,
			RepoTags:    image.RepoTags,
			RepoDigests: image.RepoDigests,
			LastLocal:   inspect.Metadata.LastTagTime,
		})
	}
	return candidates, nil
}

// remove drops an image by removing each of its references first and then the
// image itself.
//
// Removing by ID alone fails on an image that carries more than one tag —
// a development image is tagged both :local and :dev-<hash> — and the only way
// past that with a single call is Force, which would also override the daemon's
// own refusal to delete an image a container is using. Untagging first needs no
// force at all, so that last safeguard stays in place. Under the containerd
// image store dropping the final reference already reclaims the image, which is
// why a not-found on the final call is success.
func remove(ctx context.Context, cli *client.Client, candidate Candidate) error {
	for _, reference := range candidate.RepoTags {
		if _, err := cli.ImageRemove(ctx, reference, client.ImageRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove image reference %s: %w", reference, err)
		}
	}
	if _, err := cli.ImageRemove(ctx, candidate.ID, client.ImageRemoveOptions{PruneChildren: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove image %s: %w", candidate.ID, err)
	}
	return nil
}
