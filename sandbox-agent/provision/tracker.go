package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

// Tracker is what a run reports as it goes, and the box's provision.json
// (ADR 26-10-16-423 §6): every change replaces the file whole, so a reader
// never sees it torn. The agent's readiness gate waits on it, and its status
// report carries it.
type Tracker struct {
	// path is where the document is written; empty writes nothing.
	path   string
	logger *slog.Logger

	mu     sync.Mutex
	status provisionspec.Status
	// readied is closed once readiness is decided, either way.
	readied chan struct{}
	// changed wakes feature waiters after each status update.
	changed chan struct{}
	// readyDecided is whether it has been.
	readyDecided bool
	// running is whether a run is still provisioning.
	running bool
}

// NewTracker is a tracker writing path, or nothing when path is empty. Its
// document is pending until a run starts.
func NewTracker(path string) *Tracker {
	return &Tracker{
		path:    path,
		status:  provisionspec.Status{State: provisionspec.StatePending, Features: map[string]provisionspec.FeatureStatus{}},
		readied: make(chan struct{}),
		changed: make(chan struct{}),
	}
}

// SetLogger sets where a failed write of the document is logged.
func (t *Tracker) SetLogger(logger *slog.Logger) {
	t.logger = logger
}

// Status is the document as it stands.
func (t *Tracker) Status() provisionspec.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	status := t.status
	status.Features = maps.Clone(t.status.Features)
	return status
}

// AwaitReady blocks until readiness is decided, and is the error that failed
// it, if one did.
func (t *Tracker) AwaitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.readied:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.status.Ready {
		return errors.New(t.status.Error)
	}
	return nil
}

// AwaitFeatures waits for the named features to finish provisioning. An empty
// list waits for the whole run, including async features; disabled features
// do not prevent that default. An explicitly required disabled feature fails.
// As with Wait, features absent from a started plan belong to the image.
func (t *Tracker) AwaitFeatures(ctx context.Context, features []string) error {
	for {
		t.mu.Lock()
		changed := t.changed
		var problems []error
		pending := t.status.State == provisionspec.StatePending
		if len(features) == 0 {
			pending = !t.status.State.Done()
			if !pending {
				for _, name := range slices.Sorted(maps.Keys(t.status.Features)) {
					if t.status.Features[name].State == provisionspec.StateFailed {
						problems = append(problems, Problem(t.status, name))
					}
				}
			}
		} else {
			for _, name := range features {
				if feature, ok := t.status.Features[name]; ok {
					pending = pending || !feature.State.Done()
					if err := Problem(t.status, name); err != nil {
						problems = append(problems, err)
					}
				}
			}
		}
		// A failed readiness can mean no plan could be fetched or constructed.
		if t.readyDecided && !t.status.Ready && (len(features) == 0 && !pending || len(t.status.Features) == 0) {
			problems = append(problems, fmt.Errorf("provision the sandbox: %s", t.status.Error))
		}
		t.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(problems) > 0 {
			return errors.Join(problems...)
		}
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Write writes the document as it stands, as a run does on each change.
func (t *Tracker) Write() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writeLocked()
}

// start reports every feature a run plans: off with its reason, or pending.
func (t *Tracker) start(plans []*featurePlan) {
	t.update(func(status *provisionspec.Status) {
		t.running = true
		status.Features = map[string]provisionspec.FeatureStatus{}
		for _, plan := range plans {
			feature := provisionspec.FeatureStatus{State: provisionspec.StatePending, Async: !plan.blocking}
			if plan.off != "" {
				feature = provisionspec.FeatureStatus{State: provisionspec.StateOff, Reason: plan.off}
			}
			status.Features[plan.feature.Name] = feature
		}
	})
}

// step reports the step a feature is on; empty is between steps.
func (t *Tracker) step(feature, step string) {
	t.update(func(status *provisionspec.Status) {
		current := status.Features[feature]
		if current.State.Done() {
			return
		}
		current.Step = step
		if step != "" {
			current.State = provisionspec.StateProvisioning
		}
		status.Features[feature] = current
	})
}

// logPath reports where a feature's log is.
func (t *Tracker) logPath(feature, path string) {
	t.update(func(status *provisionspec.Status) {
		current := status.Features[feature]
		current.Log = path
		status.Features[feature] = current
	})
}

// finish reports a feature done: ready, and how, or failed, and why.
func (t *Tracker) finish(plan *featurePlan) {
	t.update(func(status *provisionspec.Status) {
		current := status.Features[plan.feature.Name]
		current.Step = ""
		if plan.failed != nil {
			current.State, current.Error = provisionspec.StateFailed, plan.failed.Error()
		} else {
			current.State, current.Version = provisionspec.StateReady, plan.version
			switch {
			case plan.adopted:
				current.How = provisionspec.HowAdopted
			case plan.changed:
				current.How = provisionspec.HowInstalled
			default:
				current.How = provisionspec.HowSnapshot
			}
		}
		status.Features[plan.feature.Name] = current
	})
}

// ready decides readiness: reached, or failed by err.
func (t *Tracker) ready(err error) {
	t.update(func(status *provisionspec.Status) {
		if t.readyDecided {
			return
		}
		t.readyDecided = true
		status.Ready = err == nil
		if err != nil {
			status.Error = err.Error()
		}
		close(t.readied)
	})
}

// Fail reports a run that could not start, which fails readiness: one whose
// plan could not be read, or whose bundles could not be fetched.
func (t *Tracker) Fail(err error) {
	t.ready(err)
	t.finished()
}

// finished reports the run over.
func (t *Tracker) finished() {
	t.update(func(*provisionspec.Status) { t.running = false })
}

func (t *Tracker) update(change func(*provisionspec.Status)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	change(&t.status)
	t.status.State = t.state()
	t.status.UpdatedAt = time.Now().UTC()
	close(t.changed)
	t.changed = make(chan struct{})
	t.writeLocked()
}

// state is the document's own state, from its features and readiness.
func (t *Tracker) state() provisionspec.State {
	failed := t.readyDecided && !t.status.Ready
	for _, feature := range t.status.Features {
		if !feature.State.Done() {
			return provisionspec.StateProvisioning
		}
		failed = failed || feature.State == provisionspec.StateFailed
	}
	switch {
	case failed:
		return provisionspec.StateFailed
	case t.running || !t.readyDecided:
		if len(t.status.Features) == 0 && !t.running {
			return provisionspec.StatePending
		}
		return provisionspec.StateProvisioning
	}
	return provisionspec.StateReady
}

func (t *Tracker) writeLocked() {
	if t.path == "" {
		return
	}
	data, err := json.MarshalIndent(t.status, "", "  ")
	if err == nil {
		err = writeFileAtomic(t.path, append(data, '\n'), 0o644)
	}
	if err != nil {
		logger := t.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("provision: write status", "path", t.path, "error", err)
	}
}
