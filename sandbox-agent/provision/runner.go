package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/provisionspec"
)

// Result is what became of one step in a run.
type Result string

const (
	// Applied means the step had no record, or a record of another digest,
	// and was applied.
	Applied Result = "applied"
	// Reapplied means the step's record matched but its check reported
	// drift, and it was applied again.
	Reapplied Result = "reapplied"
	// Skipped means the step's record matched and its check, if any, found it
	// in place.
	Skipped Result = "skipped"
	// Unselected means the step's selector does not select this box.
	Unselected Result = "unselected"
)

// Outcome is one step's result.
type Outcome struct {
	Feature string
	Step    string
	Result  Result
}

// Command runs name with args in dir, with env added to the runner's own
// environment. An error from a command that ran and failed wraps its
// *exec.ExitError.
type Command func(ctx context.Context, dir string, env []string, name string, args ...string) error

// Runner applies features' steps to one box.
type Runner struct {
	// Root is the filesystem the steps change: "/" on a box. Paths a step
	// names are under it.
	Root     string
	Records  Records
	Platform Platform
	// Command runs the scripts and the system's tools (systemctl, groupadd,
	// apt-get); nil runs them as processes, their output on stderr.
	Command Command
	// Fetch is how a download step reads a URL; nil is HTTP through the
	// environment's proxy.
	Fetch  Fetch
	Logger *slog.Logger
	Now    func() time.Time

	aptUpdated bool
}

// Apply applies every feature, each after the features it requires. A
// feature whose step fails stops there, and the features requiring it are
// not applied; the rest still are, and every failure is returned.
func (r *Runner) Apply(ctx context.Context, features []provisionspec.Feature) ([]Outcome, error) {
	if err := r.Platform.Supported(); err != nil {
		return nil, err
	}
	ordered, err := order(features)
	if err != nil {
		return nil, err
	}
	var outcomes []Outcome
	var errs []error
	failed := map[string]bool{}
	for _, feature := range ordered {
		if i := slices.IndexFunc(feature.Spec.Requires, func(name string) bool { return failed[name] }); i >= 0 {
			failed[feature.Name] = true
			errs = append(errs, fmt.Errorf("feature %q: not applied: it requires %q, which failed", feature.Name, feature.Spec.Requires[i]))
			continue
		}
		applied, err := r.applyFeature(ctx, feature)
		outcomes = append(outcomes, applied...)
		if err != nil {
			failed[feature.Name] = true
			errs = append(errs, err)
		}
	}
	return outcomes, errors.Join(errs...)
}

// order sorts features so each comes after those it requires, and otherwise
// by name. A requirement on a feature that is not there, or a cycle, is
// refused.
func order(features []provisionspec.Feature) ([]provisionspec.Feature, error) {
	byName := map[string]provisionspec.Feature{}
	for _, feature := range features {
		byName[feature.Name] = feature
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var ordered []provisionspec.Feature
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("features require each other: %s", strings.Join(append(path, name), " → "))
		}
		state[name] = visiting
		feature := byName[name]
		for _, required := range feature.Spec.Requires {
			if _, ok := byName[required]; !ok {
				return fmt.Errorf("feature %q requires %q, which is not there", name, required)
			}
			if err := visit(required, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		ordered = append(ordered, feature)
		return nil
	}
	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func (r *Runner) applyFeature(ctx context.Context, feature provisionspec.Feature) ([]Outcome, error) {
	var outcomes []Outcome
	for _, step := range feature.Spec.Steps {
		result, err := r.applyStep(ctx, feature, step)
		if err != nil {
			return outcomes, fmt.Errorf("feature %q: step %q: %w", feature.Name, step.Name, err)
		}
		r.logger().Info("provision", "feature", feature.Name, "step", step.Name, "kind", step.Kind(), "result", result)
		outcomes = append(outcomes, Outcome{Feature: feature.Name, Step: step.Name, Result: result})
	}
	if err := r.pruneEnvProfiles(feature); err != nil {
		return outcomes, fmt.Errorf("feature %q: %w", feature.Name, err)
	}
	return outcomes, nil
}

func (r *Runner) applyStep(ctx context.Context, feature provisionspec.Feature, step provisionspec.Step) (Result, error) {
	if !r.Platform.Selects(step.When) {
		return Unselected, nil
	}
	digest, err := StepDigest(feature, step)
	if err != nil {
		return "", err
	}
	record, ok, err := r.Records.Step(feature.Name, step.Name)
	if err != nil {
		return "", fmt.Errorf("read record: %w", err)
	}
	result := Applied
	if ok && record.Digest == digest {
		if step.Check == "" {
			return Skipped, nil
		}
		inPlace, err := r.check(ctx, feature, step.Check)
		if err != nil {
			return "", err
		}
		if inPlace {
			return Skipped, nil
		}
		result = Reapplied
	}
	if err := r.apply(ctx, feature, step); err != nil {
		return "", err
	}
	if err := r.Records.RecordStep(feature.Name, step.Name, StepRecord{Digest: digest, AppliedAt: r.now()}); err != nil {
		return "", fmt.Errorf("record: %w", err)
	}
	return result, nil
}

// check runs a check script: exit 0 is in place, any other exit is drift, and
// a script that could not be run at all is an error.
func (r *Runner) check(ctx context.Context, feature provisionspec.Feature, script string) (bool, error) {
	err := r.script(ctx, feature, script)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exitErr):
		r.logger().Info("provision: drift", "feature", feature.Name, "check", script, "exit", exitErr.ExitCode())
		return false, nil
	default:
		return false, fmt.Errorf("check %s: %w", script, err)
	}
}

func (r *Runner) apply(ctx context.Context, feature provisionspec.Feature, step provisionspec.Step) error {
	switch {
	case step.File != nil:
		return r.applyFile(feature, step.File)
	case step.Systemd != nil:
		return r.applySystemd(ctx, feature, step.Systemd)
	case step.Group != nil:
		return r.applyGroup(ctx, step.Group)
	case step.Env != nil:
		return r.applyEnv(feature, step)
	case step.Download != nil:
		return r.applyDownload(ctx, step.Download)
	case step.Packages != nil:
		return r.applyPackages(ctx, step.Packages)
	case step.Run != "":
		return r.script(ctx, feature, step.Run)
	}
	return errors.New("names no kind")
}

// script runs one of the feature's scripts from the feature's directory.
func (r *Runner) script(ctx context.Context, feature provisionspec.Feature, script string) error {
	env := []string{
		"DISCOBOX_PROVISION_FEATURE=" + feature.Name,
		"DISCOBOX_PROVISION_FEATURE_DIR=" + feature.Dir,
	}
	return r.command(ctx, feature.Dir, env, filepath.Join(feature.Dir, filepath.FromSlash(script)))
}

func (r *Runner) command(ctx context.Context, dir string, env []string, name string, args ...string) error {
	if r.Command != nil {
		return r.Command(ctx, dir, env, name, args...)
	}
	return runCommand(ctx, dir, env, name, args...)
}

func runCommand(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(append([]string{name}, args...), " "), err)
	}
	return nil
}

// path is a box path under the runner's root.
func (r *Runner) path(p string) string {
	return filepath.Join(r.root(), filepath.FromSlash(p))
}

func (r *Runner) root() string {
	if r.Root == "" {
		return "/"
	}
	return r.Root
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

func (r *Runner) now() time.Time {
	if r.Now == nil {
		return time.Now().UTC()
	}
	return r.Now()
}
