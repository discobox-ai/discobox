package sandboxruntime

import (
	"context"
	"errors"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/jsonstream"
)

// layerMsg is one of the daemon's per-layer progress messages.
func layerMsg(id, status string, current, total int64) jsonstream.Message {
	msg := jsonstream.Message{ID: id, Status: status}
	if current > 0 || total > 0 {
		msg.Progress = &jsonstream.Progress{Current: current, Total: total}
	}
	return msg
}

// messages replays a recorded sequence as the iterator ImagePull hands back.
func messages(msgs ...jsonstream.Message) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {
		for _, msg := range msgs {
			if !yield(msg, nil) {
				return
			}
		}
	}
}

// A pull's byte counts must come from the download phase alone. Extracting
// reports its own current/total against the same layer id, so folding both in
// makes a pull appear to transfer nearly twice its own size and overshoot the
// total it is measured against.
func TestPullAggregatorCountsDownloadBytesOnce(t *testing.T) {
	aggregator := newPullAggregator("ghcr.io/example/sandbox:latest")
	for _, msg := range []jsonstream.Message{
		{Status: "Pulling from example/sandbox"},
		layerMsg("aaa", "Pulling fs layer", 0, 0),
		layerMsg("aaa", "Downloading", 500, 1000),
		layerMsg("aaa", "Downloading", 1000, 1000),
		layerMsg("aaa", "Download complete", 0, 0),
		layerMsg("aaa", "Extracting", 400, 1000),
		layerMsg("aaa", "Extracting", 1000, 1000),
		layerMsg("aaa", "Pull complete", 0, 0),
	} {
		aggregator.apply(msg)
	}
	got := aggregator.snapshot()
	if got.Current != 1000 || got.Total != 1000 {
		t.Fatalf("bytes = %d/%d, want 1000/1000", got.Current, got.Total)
	}
	if got.Layers != 1 || got.LayersComplete != 1 {
		t.Fatalf("layers = %d/%d complete, want 1/1", got.LayersComplete, got.Layers)
	}
	if !got.Complete() {
		t.Fatal("expected the pull to read as complete")
	}
}

// Layers already on the host are never downloaded, but they are part of what
// the user is waiting for, so they count as complete rather than as missing.
func TestPullAggregatorCountsExistingLayersComplete(t *testing.T) {
	aggregator := newPullAggregator("img")
	aggregator.apply(layerMsg("aaa", "Already exists", 0, 0))
	aggregator.apply(layerMsg("bbb", "Downloading", 50, 100))
	got := aggregator.snapshot()
	if got.Layers != 2 || got.LayersComplete != 1 {
		t.Fatalf("layers = %d/%d complete, want 1/2", got.LayersComplete, got.Layers)
	}
	if got.Current != 50 || got.Total != 100 {
		t.Fatalf("bytes = %d/%d, want 50/100", got.Current, got.Total)
	}
	// One of two layers done is not a finished pull, even though every layer
	// heard about so far that can be complete is.
	if got.Complete() {
		t.Fatal("a pull with a layer still downloading must not read as complete")
	}
}

// The totals grow as the manifest is walked, so a snapshot is a ratio at a
// moment rather than progress towards a fixed target. A client that assumed
// otherwise would show a bar that jumps backwards.
func TestPullAggregatorTotalsGrowAsLayersAppear(t *testing.T) {
	aggregator := newPullAggregator("img")
	aggregator.apply(layerMsg("aaa", "Downloading", 100, 100))
	first := aggregator.snapshot()
	aggregator.apply(layerMsg("bbb", "Downloading", 0, 900))
	second := aggregator.snapshot()

	if first.Total != 100 {
		t.Fatalf("first total = %d, want 100", first.Total)
	}
	if second.Total != 1000 {
		t.Fatalf("second total = %d, want 1000", second.Total)
	}
	if second.Current != first.Current {
		t.Fatalf("current moved from %d to %d on a message carrying no new bytes", first.Current, second.Current)
	}
}

// fakeClock advances only when the test says so, so the throttle is asserted
// rather than slept through.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

// The daemon emits per layer per transition — hundreds of messages a second on
// a fast link — and this feeds a status line. Reports are throttled, and the
// final aggregate always goes out so the last thing a client sees is the whole
// pull rather than wherever the throttle stopped.
func TestConsumePullProgressThrottlesAndAlwaysReportsTheEnd(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)}
	var reports []PullProgress
	err := consumePullProgress(
		context.Background(),
		messages(
			layerMsg("aaa", "Downloading", 10, 100),
			layerMsg("aaa", "Downloading", 20, 100),
			layerMsg("aaa", "Downloading", 30, 100),
			layerMsg("aaa", "Pull complete", 0, 0),
		),
		"img",
		func(p PullProgress) { reports = append(reports, p) },
		clock.now,
	)
	if err != nil {
		t.Fatalf("consumePullProgress: %v", err)
	}
	// The clock never advanced, so only the first message reports, plus the
	// mandatory final aggregate.
	if len(reports) != 2 {
		t.Fatalf("reports = %d (%+v), want 2: the first and the final", len(reports), reports)
	}
	final := reports[len(reports)-1]
	if !final.Done {
		t.Fatal("the final report must be marked done")
	}
	if final.Current != 100 || final.LayersComplete != 1 {
		t.Fatalf("final = %d bytes, %d layers complete, want 100 and 1", final.Current, final.LayersComplete)
	}
}

func TestConsumePullProgressReportsAgainOnceTheIntervalPasses(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)}
	var reports []PullProgress
	msgs := []jsonstream.Message{
		layerMsg("aaa", "Downloading", 10, 100),
		layerMsg("aaa", "Downloading", 20, 100),
	}
	err := consumePullProgress(
		context.Background(),
		func(yield func(jsonstream.Message, error) bool) {
			for i, msg := range msgs {
				if i > 0 {
					clock.at = clock.at.Add(pullProgressInterval)
				}
				if !yield(msg, nil) {
					return
				}
			}
		},
		"img",
		func(p PullProgress) { reports = append(reports, p) },
		clock.now,
	)
	if err != nil {
		t.Fatalf("consumePullProgress: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("reports = %d, want 3: two throttled reports and the final", len(reports))
	}
}

// A pull whose every layer was already present reports nothing: there was no
// wait to explain, and a lone "done" would be noise.
func TestConsumePullProgressSaysNothingWhenThereWasNoPull(t *testing.T) {
	reported := false
	err := consumePullProgress(context.Background(), messages(), "img", func(PullProgress) { reported = true }, nil)
	if err != nil {
		t.Fatalf("consumePullProgress: %v", err)
	}
	if reported {
		t.Fatal("expected no progress reports for an empty stream")
	}
}

// A pull that fails mid-stream must surface as an error rather than as a
// truncated progress report that looks like a stall.
func TestConsumePullProgressSurfacesStreamErrors(t *testing.T) {
	want := errors.New("connection reset")
	err := consumePullProgress(
		context.Background(),
		func(yield func(jsonstream.Message, error) bool) {
			// A sequence must stop when the consumer does, so the first yield's
			// verdict is honored rather than assumed.
			if !yield(layerMsg("aaa", "Downloading", 10, 100), nil) {
				return
			}
			yield(jsonstream.Message{}, want)
		},
		"img",
		func(PullProgress) {},
		nil,
	)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// The daemon reports a failed pull in-band, as an errorDetail on an otherwise
// ordinary message.
func TestConsumePullProgressSurfacesDaemonErrors(t *testing.T) {
	err := consumePullProgress(
		context.Background(),
		messages(jsonstream.Message{Error: &jsonstream.Error{Message: "manifest unknown"}}),
		"ghcr.io/example/missing:latest",
		func(PullProgress) {},
		nil,
	)
	if err == nil {
		t.Fatal("expected the daemon's error to surface")
	}
	if got := err.Error(); !strings.Contains(got, "manifest unknown") {
		t.Fatalf("err = %q, want it to name the daemon's reason", got)
	}
}
