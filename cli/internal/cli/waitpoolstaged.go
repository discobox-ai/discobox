package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

// Waiting out a first run, once, at the moment the server is launched.
//
// A pool stages the images a sandbox will want as soon as it comes up (ADR
// 0069), and staging is a condition rather than a state: nothing blocks on it
// server-side, and a sandbox that wants an unstaged image simply pulls it. That
// is right for the server and wrong for the person who has just typed their
// first command, who would otherwise meet those gigabytes one at a time, inside
// whichever operation happened to need them first.
//
// So the client that launched the server waits here instead. It is the one
// caller that knows this is a first run: it started the thing.

const (
	// poolStagePollInterval is how often the pool is re-read while waiting.
	// Staging republishes its byte counts about twice a second, so this is at
	// most one report behind.
	poolStagePollInterval = time.Second
	// poolStageStallTimeout gives up on a wait that has stopped moving. Bounded
	// by silence, not by total time: staging pulls gigabytes over whatever
	// connection the user has, and there is no honest total to guess.
	poolStageStallTimeout = 5 * time.Minute
)

// waitForStagedPools waits until every pool in the project has staged its
// images, narrating what it is waiting for.
//
// It never fails the command. Staging is a head start, and a head start that
// did not happen leaves the user exactly where they would have been without
// any of this — so a stall, an error, or a pool that never stages costs the
// wait and nothing else. Returning an error here would make a registry outage
// fail `discobox ls`.
func (a *App) waitForStagedPools(ctx context.Context, report func(string)) {
	client, err := a.apiClient()
	if err != nil {
		return
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return
	}
	ticker := time.NewTicker(poolStagePollInterval)
	defer ticker.Stop()
	stall := sandboxcreate.NewStallClock(poolStageStallTimeout)
	last := ""
	for {
		pools, ok := a.readPools(ctx, client, projectID)
		if ok {
			if stagedEverywhere(pools) {
				return
			}
			if line := stagingLine(pools); line != "" && line != last {
				last = line
				if report != nil {
					report(line)
				}
				// Something moved, so the clock starts again.
				stall.Progressed()
			}
		}
		if stall.Expired() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) readPools(ctx context.Context, client *apiclientgen.Client, projectID string) ([]apimodel.Pool, bool) {
	res, err := client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: projectID})
	if err != nil {
		return nil, false
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return nil, false
	}
	return body.GetPools(), true
}

// stagedEverywhere reports whether there is nothing left to wait for.
//
// A project with no pools is staged: there is no host to stage onto, and
// waiting for one to appear would hang the first command on a project that
// deliberately has none.
func stagedEverywhere(pools []apimodel.Pool) bool {
	for i := range pools {
		if !pools[i].ImagesStaged.Or(false) {
			return false
		}
	}
	return true
}

// stagingLine says what first-time setup is doing, in the words of somebody who
// has just installed this and does not yet know what any of it is.
//
// No pool is named, and the word "pool" does not appear. It used to lead with
// one — "Default: waiting for Docker in the VM" — which named an internal
// concept, gave it an internal identifier, and spent the whole line on both. A
// user seeing this has run one command and is being told about a thing they
// have never heard of.
//
// What is left is what anybody understands: something is being set up, and
// some large files are downloading.
func stagingLine(pools []apimodel.Pool) string {
	for i := range pools {
		pool := &pools[i]
		if pool.ImagesStaged.Or(false) {
			continue
		}
		if stage, ok := pool.ImageStage.Get(); ok {
			return stageLine(stage)
		}
		// Staging has not started, so the host is still being built. The only
		// part of that worth reporting is a download, because it is long and
		// because bytes mean the same thing to everybody; the rest — booting,
		// waiting for a daemon, starting an agent — is setup, and saying which
		// kind explains nothing to somebody who does not know what any of them
		// are for.
		return setupLine(pool)
	}
	return ""
}

// setupMessage is what a host still being built says about itself.
//
// "resource pool" rather than the bare "Initializing" it briefly was: on its
// own that names no subject at all, and the reader is entitled to know what is
// being initialized. It is also not the pool's own name, which is what this
// used to lead with — "Default" identifies something the reader has never been
// introduced to, where "resource pool" at least describes one.
const setupMessage = "Initializing resource pool"

// setupLine describes a host that is still being built.
func setupLine(pool *apimodel.Pool) string {
	progress, ok := pool.ProvisionProgress.Get()
	if !ok {
		return setupMessage
	}
	// Freshness matters as much here as anywhere: the record is never cleared,
	// so a host that finished yesterday still carries the phase it finished in.
	if observed, ok := pool.ProvisionProgressAt.Get(); ok {
		if time.Since(observed) > sandboxcreate.ProvisionProgressFresh {
			return setupMessage
		}
	}
	pull, ok := progress.Pull.Get()
	if !ok {
		return setupMessage
	}
	// Named by what it is for, not by what it is called. The references here
	// are "discobox-vm" and "discobox-pool-agent", which put the internals back
	// on the screen the moment they are printed — and neither tells a first-time
	// user anything the label does not.
	label := "Downloading"
	switch progress.Phase {
	case apiclientgen.PoolProvisionPhaseFetchingVMImage:
		label = "Downloading virtual machine image"
	case apiclientgen.PoolProvisionPhasePullingPoolImage:
		label = "Downloading runtime image"
	}
	return label + bytesSuffix(pull.Current.Or(0), pull.Total.Or(0), pull.LayersComplete.Or(0), pull.Layers.Or(0))
}

// bytesSuffix is the "— 203.3 MiB of 1.9 GiB, 4/41 layers" tail two callers
// share. Neither ratio is progress toward a fixed target — both totals grow
// while the manifest is walked — so this is a pair of counts and never a
// percentage, which would visibly go backwards.
func bytesSuffix(current, total int64, layersDone, layers int) string {
	var tail string
	switch {
	case total > 0:
		tail = fmt.Sprintf(" — %s of %s", humanBytes(current), humanBytes(total))
	case current > 0:
		tail = fmt.Sprintf(" — %s", humanBytes(current))
	}
	if layers > 0 {
		tail += fmt.Sprintf(", %d/%d layers", layersDone, layers)
	}
	return tail
}

// stageLine renders one pool's staging condition.
func stageLine(stage apimodel.PoolImageStage) string {
	switch stage.GetState() {
	case apiclientgen.PoolImageStateFailed:
		if reason := strings.TrimSpace(stage.Error.Or("")); reason != "" {
			return "Could not download images: " + reason
		}
		return "Could not download images"
	case apiclientgen.PoolImageStateReady:
		return "Images ready"
	}
	// The image count leads, and says what it counts.
	//
	// It used to trail the whole line as a bare "(1 of 4)", after an image
	// reference, a byte ratio and a layer ratio — three other pairs of numbers,
	// none of them images. Which of the four it counted was anybody's guess.
	// The image count leads, and says what it counts.
	//
	// It used to trail the whole line as a bare "(1 of 4)", after an image
	// reference, a byte ratio and a layer ratio — three other pairs of numbers,
	// none of them images. Which of the four it counted was anybody's guess.
	line := "Downloading images"
	if total := stage.GetTotal(); total > 0 {
		line += fmt.Sprintf(" (%d of %d)", stage.GetDone()+1, total)
	}
	image := strings.TrimSpace(stage.Image.Or(""))
	if image == "" {
		return line
	}
	// Everything past the colon describes the one image being named.
	return line + ": " + shortImage(image) +
		bytesSuffix(stage.Current.Or(0), stage.Size.Or(0), stage.LayersComplete.Or(0), stage.Layers.Or(0))
}

// shortImage is the part of a reference that identifies it. A status line has
// one line, and the registry and namespace are the same for every image here.
func shortImage(image string) string {
	if slash := strings.LastIndex(image, "/"); slash >= 0 {
		return image[slash+1:]
	}
	return image
}

// humanBytes renders a byte count the way a person reads one. Neither count is
// progress toward a fixed target — both grow while the manifest is walked — so
// this is a pair of counts and never a percentage, which would go backwards.
func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value, exp := float64(bytes)/unit, 0
	for value >= unit && exp < 3 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGT"[exp])
}

// stagingUpdates runs the wait in the background and publishes what it would
// have printed, for a front end that shows it rather than blocking on it.
//
// The channel is closed when staging finishes, which is how the front end
// learns to take its line down — the same signal as the status line clearing,
// carried differently.
//
// Buffered by one and dropping when full: this feeds a display, and a reader
// that is briefly busy should cost the caller a skipped frame rather than
// stalling the wait behind it.
func (a *App) stagingUpdates(ctx context.Context) <-chan string {
	updates := make(chan string, 1)
	go func() {
		defer close(updates)
		a.waitForStagedPools(ctx, func(line string) {
			select {
			case updates <- line:
			default:
			}
		})
	}()
	return updates
}
