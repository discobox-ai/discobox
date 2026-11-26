package provision

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// ProgressDir holds the progress file of each step running at boot (ADR
// 26-11-24-336 §2): root's, and emptied as each step ends.
const ProgressDir = "/run/discobox/provision/progress"

// NetDev is the kernel's count of what each of the box's network interfaces
// has received (ADR 26-11-24-336 §4).
const NetDev = "/proc/net/dev"

// progressSample is how often a running step's progress is read: its file,
// and what the box has received. What is published is spaced further by the
// tracker (ProgressSpacing).
var progressSample = time.Second

// receivedFloor is how much more than it had the box must receive within
// receivedWindow for what it received to move. The pool's own calls into the
// box — its check-in, the status wait it holds, the reads a progress publish
// wakes — cross the same interfaces, a few KiB at a time; counted, they would
// show a hung step as receiving, and each publish they moved would wake the
// pool to make more of them.
const (
	receivedFloor  = 32 << 10
	receivedWindow = 10 * time.Second
)

// progressTail bounds how much of a progress file is read for its last line.
// apt writes a line per package and phase, so the file grows through a step.
const progressTail = 16 << 10

// stepProgress is what one running step has got through: what its script
// wrote to its progress file, what the agent's own step said (a download's
// count, apt's phase), and what the box has received since it started. It
// samples them until the step ends and tells the tracker. A nil one is a run
// that reports no progress, and every method of it does nothing.
type stepProgress struct {
	tracker       *Tracker
	feature, step string
	// file is the step's progress file, what its scripts are told
	// (provisionspec.ProgressEnv); empty is none.
	file   string
	netDev string
	now    func() time.Time
	logger *slog.Logger

	mu sync.Mutex
	// own is what the agent's own step said, ahead of the file's.
	own *provisionspec.Progress
	// base is the box's received count as the step started, last the count
	// last read, and moved the count when what was received last moved, at
	// receivedAt: what is reported, so traffic under the floor changes
	// nothing. window is the count at windowAt, where the growth the floor
	// is held to is measured from. counted is whether the box's count has
	// been read at all.
	base, last, moved, window int64
	counted                   bool
	receivedAt, windowAt      time.Time

	stop chan struct{}
	done chan struct{}
}

// watchStep starts sampling a step's progress, or is nil when the runner
// reports none: neither a ProgressDir nor a NetDev. A progress file that cannot
// be made leaves the step without one; its scripts are told nothing, and run
// as they would without the agent.
func (r *Runner) watchStep(plan *featurePlan, step string) *stepProgress {
	if r.ProgressDir == "" && r.NetDev == "" {
		return nil
	}
	p := &stepProgress{
		tracker: r.tracker(),
		feature: plan.feature.Name,
		step:    step,
		netDev:  r.NetDev,
		now:     r.now,
		logger:  r.logger(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if r.ProgressDir != "" {
		file := filepath.Join(r.ProgressDir, plan.feature.Name+"--"+step)
		err := os.MkdirAll(r.ProgressDir, 0o755)
		if err == nil {
			// Truncated: what a step that ran before under this name said is
			// not this one's.
			err = os.WriteFile(file, nil, 0o600)
		}
		if err != nil {
			r.logger().Warn("provision: progress file", "feature", plan.feature.Name, "step", step, "error", err)
		} else {
			p.file = file
		}
	}
	if count, counted := p.received(); counted {
		p.count(count, p.now())
	}
	go p.run()
	return p
}

// env is what the step's scripts are told: where to report.
func (p *stepProgress) env() []string {
	if p == nil || p.file == "" {
		return nil
	}
	return []string{provisionspec.ProgressEnv + "=" + p.file}
}

// report is what the agent's own step says it is doing, ahead of anything
// its file says; nil leaves the file to say it.
func (p *stepProgress) report(progress *provisionspec.Progress) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.own = progress
	p.mu.Unlock()
}

// end stops sampling and removes the step's progress file. The tracker drops
// the step's progress as the next step starts, or the feature finishes.
func (p *stepProgress) end() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.done
	if p.file != "" {
		if err := os.Remove(p.file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.logger.Warn("provision: remove progress file", "path", p.file, "error", err)
		}
	}
}

func (p *stepProgress) run() {
	defer close(p.done)
	ticker := time.NewTicker(progressSample)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.tracker.progress(p.feature, p.step, p.sample())
		}
	}
}

// sample is the step's progress as it stands.
func (p *stepProgress) sample() provisionspec.Progress {
	var progress provisionspec.Progress
	if p.file != "" {
		if line, ok := lastProgressLine(p.file); ok {
			progress = line
		}
	}
	count, counted := p.received()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.own != nil {
		progress = *p.own
	}
	if counted {
		p.count(count, p.now())
	}
	progress.Detail = provisionspec.BoundProgressDetail(progress.Detail)
	if p.counted {
		progress.Received, progress.ReceivedAt = max(p.moved-p.base, 0), p.receivedAt.UTC()
	}
	if progress.Total > 0 && progress.Total < progress.Done {
		progress.Total = 0
	}
	return progress
}

// count takes in the box's received count, read at now. The caller holds mu,
// or is the only one with p.
func (p *stepProgress) count(count int64, now time.Time) {
	switch {
	case !p.counted:
		p.base, p.last, p.moved, p.window = count, count, count, count
		p.receivedAt, p.windowAt, p.counted = now, now, true
		return
	case count < p.last:
		// An interface went, and its count with it: what it had received
		// stays received.
		dropped := p.last - count
		p.base, p.moved, p.window = p.base-dropped, p.moved-dropped, p.window-dropped
	}
	p.last = count
	switch {
	case count-p.window >= receivedFloor:
		p.moved, p.receivedAt = count, now
		p.window, p.windowAt = count, now
	case now.Sub(p.windowAt) >= receivedWindow:
		p.window, p.windowAt = count, now
	}
}

// received is what the box's interfaces, loopback aside, have received, and
// false when it cannot be read.
func (p *stepProgress) received() (int64, bool) {
	if p.netDev == "" {
		return 0, false
	}
	data, err := os.ReadFile(p.netDev)
	if err != nil {
		return 0, false
	}
	return receivedBytes(data)
}

// receivedBytes sums the received bytes of /proc/net/dev's interfaces but
// loopback: after two header lines, NAME: RX-BYTES RX-PACKETS ….
func receivedBytes(netDev []byte) (int64, bool) {
	var total int64
	counted := false
	for line := range strings.SplitSeq(string(netDev), "\n") {
		name, counters, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(counters)
		if name == "lo" || len(fields) == 0 {
			continue
		}
		bytes, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		total += bytes
		counted = true
	}
	return total, counted
}

// lastProgressLine is the last line of a progress file that says something.
func lastProgressLine(path string) (provisionspec.Progress, bool) {
	f, err := os.Open(path)
	if err != nil {
		return provisionspec.Progress{}, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return provisionspec.Progress{}, false
	}
	offset := max(info.Size()-progressTail, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, offset, progressTail))
	if err != nil {
		return provisionspec.Progress{}, false
	}
	if offset > 0 {
		// The first line read is likely cut: start after it.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	// A line still being written is not read until it ends.
	data = data[:bytes.LastIndexByte(data, '\n')+1]
	var last provisionspec.Progress
	found := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 4096), progressTail+1)
	for scanner.Scan() {
		if progress, ok := provisionspec.ParseProgressLine(scanner.Text()); ok {
			last, found = progress, true
		}
	}
	return last, found
}
