// Package imagereap reclaims disk on a Docker daemon by removing Discobox
// images that nothing uses any more (ADR 0040).
//
// Two things make an image reclaimable, and both are required. The image must
// carry harness.ReclaimLabel, which is the boundary of what Discobox is entitled
// to delete on a daemon it shares with a developer's own images; and it must
// have arrived on this daemon longer ago than the retention window without any
// container, running or stopped, referring to it.
//
// "Arrived" is the daemon's own LastTagTime, not the image's Created timestamp.
// Created is when whoever published the image built it, so a release image built
// weeks before it shipped would be stale the instant it was pulled. Which
// operations set LastTagTime depends on the image store. The containerd store
// sets it on build, pull, load and tag, except that a build clamped to
// SOURCE_DATE_EPOCH takes that epoch for both, and an arrival no later than the
// build is that clamp, not an arrival: an unknown age (ADR 0143). The classic
// store (overlay2) sets it only on a tag, so every image it loaded or pulled has
// an unknown age, and applying a tag to itself again sets it to now.
//
// That is how an unknown age gets a clock (ADR 26-11-13-655). The image cache
// load and the pulls that bring a pool its sandbox base image stamp it as it
// lands (Stamp), and every pass stamps each labeled image still without one,
// whatever brought it, so its retention window starts at the first pass that
// sees it. On the containerd store the stamp changes nothing,
// and a clamped build stays kept by the ordinary pass. An explicit clear
// (Options.Clear) counts an unknown age as older than its window instead.
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
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/harness"
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
	retention, err := configuredRetention()
	if err != nil {
		return 0, err
	}
	if retention == 0 {
		return DefaultRetention, nil
	}
	return retention, nil
}

// configuredRetention is RetentionFromEnv without the default applied: it
// returns zero when RetentionEnv is unset. RetentionFromEnv is its only caller.
//
// The distinction between "not configured" and "configured to the default
// value" is load-bearing in the server, not here: the server keeps it in
// config.Config.ImageRetention, which records an override in each pool
// container's configuration and would change every pool's revision if it
// materialized a default (ADR 0096 server config §5).
func configuredRetention() (time.Duration, error) {
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
	// Zero means the daemon did not report one, which is an unknown age: an
	// ordinary pass keeps it, and only a clear removes it.
	LastLocal time.Time
	// Created is when the image was built. It is never an age, only the floor
	// an arrival has to be after to be one (knownArrival).
	Created time.Time
}

// knownArrival reports whether LastLocal is a time this image arrived here.
//
// It is not when the daemon reported none, and not when it is no later than
// Created: nothing arrives before it was built, and a build clamped to
// SOURCE_DATE_EPOCH — the Nix dev shell sets 1980 — is stamped with that epoch
// for both on the containerd image store. Taken as an age, that makes the image
// decades old the moment it is built (ADR 0143).
func (c Candidate) knownArrival() bool {
	return !c.LastLocal.IsZero() && c.LastLocal.After(c.Created)
}

// references returns every way this image can be named, so a keep entry matches
// whether it was written as an ID, a tag, or a digest reference. Tags and
// digests are normalized (normalizedReferences), as keep entries are.
func (c Candidate) references() []string {
	refs := make([]string, 0, 2*(len(c.RepoTags)+len(c.RepoDigests))+1)
	refs = append(refs, c.ID)
	for _, ref := range append(append([]string{}, c.RepoTags...), c.RepoDigests...) {
		refs = append(refs, normalizedReferences(ref)...)
	}
	return refs
}

// normalizedReferences returns the forms a reference matches an image by: as
// written, and, when it parses, its tag and its digest each on the
// fully-qualified repository. So a keep entry written `name:tag@digest`, or
// `docker.io/library/name:tag`, matches the image a daemon reports as
// `name:tag` or `name@digest`, as the floor's repositoryOf already does.
func normalizedReferences(ref string) []string {
	forms := []string{ref}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return forms
	}
	if tagged, ok := named.(reference.Tagged); ok {
		forms = append(forms, named.Name()+":"+tagged.Tag())
	}
	if digested, ok := named.(reference.Digested); ok {
		forms = append(forms, named.Name()+"@"+digested.Digest().String())
	}
	return forms
}

// Reclaimable returns the candidates that may be removed.
//
// inUse holds the image IDs containers refer to, keepReferences names images
// that must survive regardless of age by ID or any reference, and now is the clock. An image is
// reclaimable only when it is in neither set, is not the newest of a repository
// keep names, and its local age exceeds retention. An image with no known age
// is kept, unless clearing is set: an explicit clear counts it as older than any
// window (ADR 26-11-13-655 §4).
func Reclaimable(candidates []Candidate, inUse map[string]struct{}, keepReferences []string, retention time.Duration, now time.Time, clearing bool) []Candidate {
	keep := keepSet(keepReferences)
	newest := newestPerKeptRepository(candidates, keep)
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
		if _, current := newest[candidate.ID]; current {
			continue
		}
		if !candidate.knownArrival() {
			// No knowable local age. An ordinary pass waits for its stamp to
			// start the clock rather than guess: on the containerd store this is
			// a build clamped a moment ago (ADR 0143). A clear is asked to remove
			// what is unused, and does.
			if clearing {
				reclaimable = append(reclaimable, candidate)
			}
			continue
		}
		if now.Sub(candidate.LastLocal) < retention {
			continue
		}
		reclaimable = append(reclaimable, candidate)
	}
	return reclaimable
}

// newestPerKeptRepository returns the IDs of the newest image in each
// repository a keep reference names.
//
// This is the floor under the keep set, and it exists because deletion is
// irreversible while a keep set is a snapshot somebody else took: the server
// loads the development manifest once at startup, so an image built after that
// is in no keep set at all, and during a rebuild pass — which takes far longer
// than the development window — the manifest on disk still names the
// *previous* build. Age then makes the freshly built image look like garbage
// precisely because it is the one being built on, and reclaiming it strands
// the watcher, which only rebuilds when a source file changes.
//
// The newest image of such a repository is the current one by construction:
// that is what the mutable tag (`:local`, `:latest`) points at and what the
// next build layers on. A repository no keep reference names gets no floor
// (ADR 26-11-13-655 §3): nothing current is in it, as with every per-harness
// repository since every box runs the sandbox base, and its newest image is
// reclaimed on usage and age like any other.
//
// Newest is by known arrival. A repository with none is ranked by Created
// instead, so an unknown age still leaves its latest build. A tie protects
// every image in it rather than picking one.
func newestPerKeptRepository(candidates []Candidate, keep map[string]struct{}) map[string]struct{} {
	kept := keptRepositories(keep)
	if len(kept) == 0 {
		return nil
	}
	arrived := map[string]time.Time{}
	built := map[string]time.Time{}
	for _, candidate := range candidates {
		for _, repository := range repositories(candidate) {
			if _, ok := kept[repository]; !ok {
				continue
			}
			if at, ok := built[repository]; !ok || candidate.Created.After(at) {
				built[repository] = candidate.Created
			}
			if !candidate.knownArrival() {
				continue
			}
			if at, ok := arrived[repository]; !ok || candidate.LastLocal.After(at) {
				arrived[repository] = candidate.LastLocal
			}
		}
	}
	current := map[string]struct{}{}
	for _, candidate := range candidates {
		for _, repository := range repositories(candidate) {
			if _, ok := kept[repository]; !ok {
				continue
			}
			var newest bool
			if at, ok := arrived[repository]; ok {
				newest = candidate.knownArrival() && candidate.LastLocal.Equal(at)
			} else {
				newest = candidate.Created.Equal(built[repository])
			}
			if newest {
				current[candidate.ID] = struct{}{}
				break
			}
		}
	}
	return current
}

// keptRepositories returns the repository of each keep reference that names
// one. A keep entry that is an image ID names no repository.
func keptRepositories(keep map[string]struct{}) map[string]struct{} {
	repositories := map[string]struct{}{}
	for reference := range keep {
		if reference == "" || strings.HasPrefix(reference, "sha256:") {
			continue
		}
		if name := repositoryOf(reference); name != "" {
			repositories[name] = struct{}{}
		}
	}
	return repositories
}

// repositories returns the repository of each of an image's tags. An untagged
// image belongs to none, so nothing protects it as "current".
func repositories(candidate Candidate) []string {
	names := make([]string, 0, len(candidate.RepoTags))
	for _, tag := range candidate.RepoTags {
		if name := repositoryOf(tag); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// repositoryOf is a reference's repository, without its tag or digest.
//
// It is normalized, so the short name a daemon reports a Docker Hub image by
// and the fully qualified one a configuration may name it by compare equal. A
// reference that does not parse falls back to stripping the tag, where the
// colon must come after the last slash or it is a registry port, not a tag.
func repositoryOf(ref string) string {
	if named, err := reference.ParseNormalizedNamed(ref); err == nil {
		return named.Name()
	}
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon]
	}
	return ref
}

// keepSet indexes keep references by every form they match an image by
// (normalizedReferences).
func keepSet(references []string) map[string]struct{} {
	keep := make(map[string]struct{}, 2*len(references))
	for _, ref := range references {
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		for _, form := range normalizedReferences(ref) {
			keep[form] = struct{}{}
		}
	}
	return keep
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
	// Clear marks an explicit clear (`discobox admin pool clear-cache`): an
	// image with no known age counts as older than Retention and is removed if
	// nothing uses or keeps it, and the pass stamps nothing, since a stamp
	// would only make such an image fresh (ADR 26-11-13-655 §4).
	Clear bool
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
// An ordinary pass first stamps every labeled image the daemon reports no
// arrival for (stampUnknownAges), so an image that arrived on the classic
// store starts its window here instead of being kept forever.
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

	if !opts.Clear {
		stampUnknownAges(ctx, cli, candidates, keepSet(opts.Keep), logger)
	}

	result := Result{Scanned: len(candidates)}
	for _, candidate := range Reclaimable(candidates, inUse, opts.Keep, retention, now, opts.Clear) {
		if err := remove(ctx, cli, candidate); err != nil {
			logger.DebugContext(ctx, "retained Discobox image that could not be reclaimed",
				"image", candidate.ID, "tags", candidate.RepoTags, "error", err)
			continue
		}
		if candidate.knownArrival() {
			logger.InfoContext(ctx, "reclaimed unused Discobox image",
				"image", candidate.ID, "tags", candidate.RepoTags,
				"age", now.Sub(candidate.LastLocal).Truncate(time.Minute))
		} else {
			logger.InfoContext(ctx, "reclaimed unused Discobox image of unknown age",
				"image", candidate.ID, "tags", candidate.RepoTags)
		}
		result.Removed = append(result.Removed, candidate.ID)
	}
	result.Retained = result.Scanned - len(result.Removed)
	return result, nil
}

// Stamp starts image's age on a daemon that did not record its arrival, by
// applying one of its tags to itself again. The image cache load and the
// server's and pool agent's pulls call it as an image lands (ADR 26-11-13-655
// §2): the classic image store sets
// LastTagTime only on a tag, so without it an image loaded or pulled there has
// no age until a pass stamps it, and an explicit clear removes it at once.
//
// On the containerd store, which records the arrival itself, the call changes
// nothing. An image with no tag cannot be stamped and is left as it is. A
// caller logs a failure and carries on: an unknown age only keeps the image
// longer.
func Stamp(ctx context.Context, cli *client.Client, image string) error {
	inspect, err := cli.ImageInspect(ctx, image)
	if err != nil {
		return fmt.Errorf("inspect image %s: %w", image, err)
	}
	return stamp(ctx, cli, stampTag(image, inspect.RepoTags))
}

// stampTag is the tag Stamp applies: the reference the image arrived under
// when that is one of its tags, and otherwise its first, as for a reference
// pinned by digest.
func stampTag(image string, tags []string) string {
	for _, tag := range tags {
		if tag == image {
			return tag
		}
	}
	if len(tags) == 0 {
		return ""
	}
	return tags[0]
}

func stamp(ctx context.Context, cli *client.Client, tag string) error {
	if tag == "" {
		return nil
	}
	if _, err := cli.ImageTag(ctx, client.ImageTagOptions{Source: tag, Target: tag}); err != nil {
		return fmt.Errorf("apply tag %s to itself: %w", tag, err)
	}
	return nil
}

// stampUnknownAges stamps every candidate with a tag and no known arrival, and
// records the arrival the daemon then reports (ADR 26-11-13-655 §1).
//
// They are stamped oldest Created first, so within a repository the latest
// build gets the latest arrival and the floor keeps the right one. Release
// images are built clamped and share a Created, so among equal builds a kept
// one goes last and the floor lands on the image the keep set already names.
//
// That order only ranks the images stamped together. An image that already had
// an arrival — the current sandbox base, stamped as it landed, beside the older
// bases an upgraded pool still holds with none — would otherwise fall behind
// every image stamped after it, and the floor would move to an older build. So
// in a repository the floor covers, the image holding its newest arrival
// (stampOrder) is stamped again in the same order, at its own build: a pass
// ranks it above the builds older than it, and below one newer than it, as the
// floor would have had the newer build arrived stamped. That restarts only that
// image's clock, and it is the one the floor keeps anyway.
//
// On the containerd store a stamp changes nothing, and the candidate keeps its
// unknown age. A failure is logged and leaves the candidate as it was, until
// the next pass.
func stampUnknownAges(ctx context.Context, cli *client.Client, candidates []Candidate, keep map[string]struct{}, logger *slog.Logger) {
	for _, i := range stampOrder(candidates, keep) {
		candidate := &candidates[i]
		if err := stamp(ctx, cli, candidate.RepoTags[0]); err != nil {
			logger.WarnContext(ctx, "could not start the age of a Discobox image",
				"image", candidate.ID, "tags", candidate.RepoTags, "error", err)
			continue
		}
		inspect, err := cli.ImageInspect(ctx, candidate.ID)
		if err != nil {
			logger.WarnContext(ctx, "inspect a stamped Discobox image",
				"image", candidate.ID, "error", err)
			continue
		}
		candidate.LastLocal = inspect.Metadata.LastTagTime
	}
}

// stampOrder returns the candidates a pass stamps, by index, in the order it
// stamps them: every tagged image with no known arrival, and the floor's
// holders of each kept repository one of those is in, ordered by Created.
// Among equal builds a holder goes last, then a kept image, so the floor stays
// where it was or on the image the keep set names.
func stampOrder(candidates []Candidate, keep map[string]struct{}) []int {
	unknown := make([]int, 0, len(candidates))
	for i, candidate := range candidates {
		if !candidate.knownArrival() && len(candidate.RepoTags) > 0 {
			unknown = append(unknown, i)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	holders := floorHolders(candidates, unknown, keep)
	holder := make(map[int]bool, len(holders))
	for _, i := range holders {
		holder[i] = true
	}
	order := append(unknown, holders...)
	slices.SortStableFunc(order, func(a, b int) int {
		if c := candidates[a].Created.Compare(candidates[b].Created); c != 0 {
			return c
		}
		if c := cmpBool(holder[a], holder[b]); c != 0 {
			return c
		}
		return cmpBool(keepsAny(keep, candidates[a].references()), keepsAny(keep, candidates[b].references()))
	})
	return order
}

// floorHolders returns, for each kept repository one of the stamped images
// (the indexes in stamped) is in, the candidates that hold its newest known
// arrival now, before the stamps: the images the floor protects, which
// stampOrder stamps again at their own build.
func floorHolders(candidates []Candidate, stamped []int, keep map[string]struct{}) []int {
	kept := keptRepositories(keep)
	touched := map[string]struct{}{}
	for _, i := range stamped {
		for _, repository := range repositories(candidates[i]) {
			if _, ok := kept[repository]; ok {
				touched[repository] = struct{}{}
			}
		}
	}
	if len(touched) == 0 {
		return nil
	}
	current := newestPerKeptRepository(candidates, keep)
	holders := []int{}
	for i, candidate := range candidates {
		if _, ok := current[candidate.ID]; !ok || !candidate.knownArrival() || len(candidate.RepoTags) == 0 {
			continue
		}
		for _, repository := range repositories(candidate) {
			if _, ok := touched[repository]; ok {
				holders = append(holders, i)
				break
			}
		}
	}
	return holders
}

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
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

// imageInUse reports whether any container on the daemon was created from this
// image.
//
// The ancestor filter also matches containers created from images built on top
// of it, which errs in the safe direction: a base image whose descendant is
// running stays.
func imageInUse(ctx context.Context, cli *client.Client, imageID string) (bool, error) {
	filters := client.Filters{}
	filters = filters.Add("ancestor", imageID)
	containers, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return false, fmt.Errorf("list containers using image %s: %w", imageID, err)
	}
	return len(containers.Items) > 0, nil
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
		// An unparsable Created leaves it zero, which leaves the arrival to
		// stand on its own, as it did before Created was consulted.
		created, _ := time.Parse(time.RFC3339Nano, inspect.Created)
		candidates = append(candidates, Candidate{
			ID:          image.ID,
			RepoTags:    image.RepoTags,
			RepoDigests: image.RepoDigests,
			LastLocal:   inspect.Metadata.LastTagTime,
			Created:     created,
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
	// Re-checked immediately before mutating, scoped to this one image, because
	// the daemon guards the two steps below unequally: it refuses to delete an
	// image a container is using, but it will untag one without complaint. The
	// pass-level in-use set was collected before an inspect per candidate, so a
	// container created in between would be missed, and losing that race strips
	// the references off a live image — the container keeps running while
	// nothing can name its image again, and the delete then fails, leaving it
	// dangling and unnamed. The up-front set makes the pass cheap; this makes it
	// safe.
	inUse, err := imageInUse(ctx, cli, candidate.ID)
	if err != nil {
		return err
	}
	if inUse {
		return fmt.Errorf("image %s is in use by a container", candidate.ID)
	}
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
