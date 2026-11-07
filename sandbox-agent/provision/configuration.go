package provision

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"gopkg.in/yaml.v3"
)

// ConfigurationPlanPath is a boot-local selection published once bundle intake
// and proxy/trust preparation have succeeded. It is independent of feature
// completion and general exec readiness; an absent document never means off.
const ConfigurationPlanPath = "/run/discobox/configuration-plan.yaml"
const DockerConfigurationUnit = "discobox-docker-configuration.service"

// ConfigurationPlan carries the selected inputs, including explicit opt-outs.
// Only the provisioning owner publishes it, after resolving the delivered pins.
type ConfigurationPlan struct {
	Version  int `yaml:"version"`
	Features []provisionspec.Feature
	Options  Options
}

func publishConfigurationPlan(features []provisionspec.Feature, opts Options) error {
	data, err := yaml.Marshal(ConfigurationPlan{Version: 1, Features: features, Options: opts})
	if err != nil {
		return err
	}
	return writeFileAtomic(ConfigurationPlanPath, data, 0o644)
}

// WaitConfigurationPlan waits for the current boot's resolved inputs. It never
// consults provision.json, whose readiness is a different milestone.
func WaitConfigurationPlan(ctx context.Context, path string) (ConfigurationPlan, error) {
	for {
		var plan ConfigurationPlan
		data, err := os.ReadFile(path)
		if err == nil {
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			err = decoder.Decode(&plan)
			if err == nil && plan.Version != 1 {
				err = fmt.Errorf("unsupported configuration plan version %d", plan.Version)
			}
			if err == nil {
				for _, feature := range plan.Features {
					if err = feature.Spec.Validate(); err != nil {
						break
					}
				}
			}
			return plan, err
		}
		if !os.IsNotExist(err) {
			return plan, err
		}
		select {
		case <-ctx.Done():
			return plan, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

// Configure applies one selected feature's configuration without any lifecycle
// actions or waits on daemons. Its records and pending restarts are shared with
// the ordinary runner under a process-safe lock. Restarts belong to Provision.
func (r *Runner) Configure(ctx context.Context, features []provisionspec.Feature, opts Options, name string) error {
	ordered, err := order(features)
	if err != nil {
		return err
	}
	// Configuration selection is pure: do not probe unrelated tool versions or
	// prune profiles here. Neither can belong in a daemon's dependency closure.
	off := map[string]bool{}
	deferred := map[string]bool{}
	for _, feature := range ordered {
		_, off[feature.Name] = opts.Off[feature.Name]
		plan := &featurePlan{feature: feature, deferredFrom: len(feature.Spec.Steps)}
		for _, required := range feature.Spec.Requires {
			off[feature.Name] = off[feature.Name] || off[required]
			deferred[feature.Name] = deferred[feature.Name] || deferred[required]
		}
		if opts.Snapshot {
			for i, step := range feature.Spec.Steps {
				if !feature.Spec.InSnapshot(step) {
					plan.deferredFrom = i
					break
				}
			}
			if deferred[feature.Name] {
				plan.deferredFrom = 0
			}
			deferred[feature.Name] = plan.deferredFrom < len(feature.Spec.Steps)
		}
		if feature.Name != name {
			continue
		}
		if off[name] {
			return nil
		}
		if err := feature.Spec.Validate(); err != nil {
			return err
		}
		r.status = NewTracker("")
		defer closeLogs([]*featurePlan{plan})
		_, err := r.configure(ctx, plan)
		return err
	}
	return nil
}

func (r *Runner) configurationLock(ctx context.Context, name string) (*filelock.Lock, error) {
	dir := filepath.Join(r.Records.Dir, "configuration")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for {
		lock, err := filelock.TryAcquire(filepath.Join(dir, name+".lock"))
		if !errors.Is(err, filelock.ErrBusy) {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(waitPoll):
		}
	}
}

func (r *Runner) restartPath(name string) string {
	return filepath.Join(r.Records.Dir, "configuration", name+".restarts.json")
}

func (r *Runner) pendingRestarts(name string) ([]string, error) {
	data, err := os.ReadFile(r.restartPath(name))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ticket restartTicketRecord
	err = json.Unmarshal(data, &ticket)
	return ticket.Units, err
}

type restartTicketRecord struct {
	Units    []string
	Revision string
}

// acknowledgeRestarts cannot clear a newer configuration's pending restart.
// The lock is taken only after the restart has finished, never around it.
func (r *Runner) acknowledgeRestarts(ctx context.Context, name string, ticket []byte) error {
	lock, err := r.configurationLock(ctx, name)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	current, err := os.ReadFile(r.restartPath(name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(current, ticket) {
		return nil
	}
	return os.Remove(r.restartPath(name))
}

// configure's lock covers input validation, writes, records and the final
// reload. No operation in this closure can start a daemon that needs the lock.
func (r *Runner) configure(ctx context.Context, plan *featurePlan) ([]Outcome, error) {
	lock, err := r.configurationLock(ctx, plan.feature.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	out, err := r.output(plan)
	if err != nil {
		return nil, err
	}
	restart, err := r.pendingRestarts(plan.feature.Name)
	if err != nil {
		return nil, err
	}
	var outcomes []Outcome
	for i, step := range plan.feature.Spec.Steps {
		if !step.Configuration || !r.Platform.Selects(step.When) || i >= plan.deferredFrom {
			continue
		}
		// Validate on every activation, even if a snapshot brought matching records.
		// Unchanged content keeps its record and does not schedule a restart.
		same, err := r.configurationInPlace(ctx, out, plan.feature, step)
		if err != nil {
			return outcomes, fmt.Errorf("configuration %s/%s: %w", plan.feature.Name, step.Name, err)
		}
		if !same {
			// Journal before writing: an interruption after a partial write must still
			// converge an inherited running daemon on the next successful application.
			for _, unit := range step.Restarts() {
				if !r.Booted {
					continue
				}
				activeErr := r.systemctl(ctx, out, "is-active", "--quiet", unit)
				if activeErr != nil {
					var exit *exec.ExitError
					if errors.As(activeErr, &exit) && (exit.ExitCode() == 3 || exit.ExitCode() == 4) {
						continue
					}
					return outcomes, fmt.Errorf("check running %s: %w", unit, activeErr)
				}
				if !slices.Contains(restart, unit) {
					restart = append(restart, unit)
				}
			}
			data, err := json.Marshal(restartTicketRecord{Units: restart, Revision: rand.Text()})
			if err != nil {
				return outcomes, err
			}
			if err := writeFileAtomic(r.restartPath(plan.feature.Name), data, 0o644); err != nil {
				return outcomes, err
			}
			if err := r.Records.Forget(plan.feature.Name, step.Name); err != nil {
				return outcomes, err
			}
		}
		result, err := r.applyStep(ctx, out, plan.feature, step)
		if err != nil {
			return outcomes, fmt.Errorf("configuration %s/%s: %w (log: %s)", plan.feature.Name, step.Name, err, r.logPath(plan.feature.Name))
		}
		outcomes = append(outcomes, Outcome{Feature: plan.feature.Name, Step: step.Name, Result: result})
	}
	if r.Booted {
		if err := r.systemctl(ctx, out, "daemon-reload"); err != nil {
			return outcomes, fmt.Errorf("configuration %s: reload systemd: %w", plan.feature.Name, err)
		}
	}
	return outcomes, nil
}

func (r *Runner) configurationInPlace(ctx context.Context, out io.Writer, feature provisionspec.Feature, step provisionspec.Step) (bool, error) {
	if step.Run != "" {
		return r.check(ctx, out, feature, step.Check)
	}
	var path, source string
	mode := os.FileMode(0o644)
	if step.File != nil {
		path, source, mode = step.File.Path, step.File.Source, fileMode(step.File.Mode)
	} else {
		drop := step.Systemd.DropIn
		path, source = filepath.Join("/etc/systemd/system", drop.Unit+".d", drop.Name), drop.Source
	}
	want, err := os.ReadFile(filepath.Join(feature.Dir, filepath.FromSlash(source)))
	if err != nil {
		return false, err
	}
	if step.File != nil && step.File.Merge == provisionspec.MergeJSON {
		want, err = mergeJSONFile(r.path(path), want)
		if err != nil {
			return false, err
		}
	}
	have, err := os.ReadFile(r.path(path))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Stat(r.path(path))
	return err == nil && info.Mode().Perm() == mode.Perm() && bytes.Equal(have, want), err
}
