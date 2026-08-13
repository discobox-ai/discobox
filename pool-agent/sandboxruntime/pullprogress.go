package sandboxruntime

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/moby/moby/api/types/jsonstream"
)

// PullProgress is the aggregate state of one image pull, as a status line wants
// it rather than as the daemon reports it.
//
// The daemon emits a message per layer per transition, which is far too much to
// forward to a user watching a single line of output (ADR 0039): what answers
// "is this moving, and how far in is it" is bytes against bytes and layers
// against layers.
type PullProgress struct {
	// Image is the reference being pulled, so a client can say what it is
	// waiting for without being told separately.
	Image string
	// Layers is how many layers the pull has heard about so far. It grows as
	// the manifest is walked, so an early snapshot legitimately reports fewer
	// layers than the pull will finish with.
	Layers int
	// LayersComplete counts layers fully pulled, including ones already present
	// on the host.
	LayersComplete int
	// Current and Total are download bytes. Total is only the total of layers
	// whose size the daemon has reported, so it too grows during the pull; a
	// client must treat the pair as a ratio at a moment, not as a fixed target.
	Current int64
	Total   int64
	// Done reports the pull finished successfully.
	Done bool
}

// Complete reports whether every layer heard about so far is done. It is not
// the same as Done: a pull one layer into a ten-layer image is briefly
// "complete" by this measure, which is exactly why the pull's own end is
// tracked separately.
func (p PullProgress) Complete() bool {
	return p.Layers > 0 && p.LayersComplete == p.Layers
}

// pullLayer is what the aggregate needs to remember about one layer.
type pullLayer struct {
	current  int64
	total    int64
	complete bool
}

// pullAggregator folds the daemon's per-layer message stream into a
// PullProgress. It is deliberately separate from the transport so it can be
// tested against recorded message sequences.
type pullAggregator struct {
	image  string
	layers map[string]*pullLayer
}

func newPullAggregator(image string) *pullAggregator {
	return &pullAggregator{image: image, layers: map[string]*pullLayer{}}
}

// Layer statuses the daemon reports. Only these carry byte counts or finish a
// layer; the rest ("Pulling from …", "Digest: …", "Status: …") describe the
// pull as a whole and have no layer id.
const (
	pullStatusDownloading     = "Downloading"
	pullStatusDownloadDone    = "Download complete"
	pullStatusPullComplete    = "Pull complete"
	pullStatusAlreadyExists   = "Already exists"
	pullStatusExtracting      = "Extracting"
	pullStatusVerifyingChecks = "Verifying Checksum"
)

// apply folds one message in and reports whether the aggregate changed.
//
// Byte counts are taken from Downloading only. Extracting reports its own
// current/total against the same layer id, so counting both would inflate the
// numbers to nearly double and make a pull appear to overshoot its own total.
func (a *pullAggregator) apply(msg jsonstream.Message) bool {
	if msg.ID == "" {
		return false
	}
	layer, ok := a.layers[msg.ID]
	if !ok {
		layer = &pullLayer{}
		a.layers[msg.ID] = layer
		// A newly heard-of layer is itself a change worth reporting: it moves
		// the layer count even before any bytes arrive.
		ok = false
	}
	before := *layer
	switch msg.Status {
	case pullStatusDownloading:
		if msg.Progress != nil {
			layer.current = msg.Progress.Current
			if msg.Progress.Total > 0 {
				layer.total = msg.Progress.Total
			}
		}
	case pullStatusDownloadDone, pullStatusVerifyingChecks, pullStatusExtracting:
		// The bytes are in, whatever the last Downloading message said.
		if layer.total > 0 {
			layer.current = layer.total
		}
	case pullStatusPullComplete, pullStatusAlreadyExists:
		if layer.total > 0 {
			layer.current = layer.total
		}
		layer.complete = true
	}
	return !ok || *layer != before
}

func (a *pullAggregator) snapshot() PullProgress {
	progress := PullProgress{Image: a.image, Layers: len(a.layers)}
	for _, layer := range a.layers {
		progress.Current += layer.current
		progress.Total += layer.total
		if layer.complete {
			progress.LayersComplete++
		}
	}
	return progress
}

// pullProgressInterval is the floor between two progress reports for one pull.
// The daemon emits per layer per transition — hundreds of messages a second on
// a fast link — and this is a status line, not a transfer meter.
const pullProgressInterval = 500 * time.Millisecond

// consumePullProgress drains the daemon's pull stream, reporting an aggregate
// no more often than pullProgressInterval and once more when the pull ends.
//
// Draining is not optional: the pull only proceeds while its stream is being
// read, so this both reports progress and is what runs the pull to completion.
func consumePullProgress(
	ctx context.Context,
	messages iter.Seq2[jsonstream.Message, error],
	image string,
	report func(PullProgress),
	now func() time.Time,
) error {
	if now == nil {
		now = time.Now
	}
	aggregator := newPullAggregator(image)
	var lastReport time.Time
	reported := false
	for msg, err := range messages {
		if err != nil {
			return err
		}
		if msg.Error != nil {
			return fmt.Errorf("pull image %q: %s", image, msg.Error.Message)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !aggregator.apply(msg) || report == nil {
			continue
		}
		if at := now(); lastReport.IsZero() || at.Sub(lastReport) >= pullProgressInterval {
			lastReport = at
			reported = true
			report(aggregator.snapshot())
		}
	}
	// The last aggregate always goes out, so a client's final line is the whole
	// pull rather than wherever the throttle happened to stop. A pull that never
	// reported anything (every layer already present) says nothing at all.
	if report != nil && reported {
		final := aggregator.snapshot()
		final.Done = true
		report(final)
	}
	return nil
}
