package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
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
	// NotInstalled means the step installs the feature's own copy, and the
	// feature adopted the box's instead.
	NotInstalled Result = "not-installed"
	// Deferred means a snapshot is being taken and the step is left out of
	// it (snapshot: false): it is not recorded, so the box's own boot
	// applies it.
	Deferred Result = "deferred"
)

// Outcome is one step's result.
type Outcome struct {
	Feature string
	Step    string
	Result  Result
}

// Command runs name with args in dir, with env added to the runner's own
// environment and its output written to out. An error from a command that ran
// and failed wraps its *exec.ExitError.
type Command func(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error

// Runner provisions features onto one box.
type Runner struct {
	// Root is the filesystem the steps change: "/" on a box. Paths a step
	// names are under it.
	Root     string
	Records  Records
	Platform Platform
	// Command runs the scripts and the system's tools (systemctl, groupadd,
	// apt-get); nil runs them as processes.
	Command Command
	// Fetch is how a download step reads a URL; nil is HTTP through the
	// environment's proxy.
	Fetch Fetch
	// LogDir holds each feature's log, <feature>.log, which its steps write
	// their output to; empty writes it to stderr.
	LogDir string
	// Placeholders is the directory a feature's placeholder commands are
	// written to while it provisions in the background; empty writes none.
	Placeholders string
	// Self is the sandbox agent's own executable: a command resolving to it
	// is one of its personalities, never a copy a feature can adopt.
	Self string
	// Tracker is told what happens, and writes provision.json; nil tells
	// nobody.
	Tracker *Tracker
	Logger  *slog.Logger
	Now     func() time.Time

	// status is the run's tracker: Tracker, or one of its own.
	status *Tracker
	// apt is the lock every step using apt or dpkg takes.
	apt sync.Mutex
	// merge is the lock a file step merging into a file takes across its
	// read and its write: features provisioning in parallel may merge into
	// the same file (a daemon.json), and two unlocked merges lose one's keys.
	merge      sync.Mutex
	aptUpdated bool
}

// Options are what a run provisions, beyond the features themselves.
type Options struct {
	// Off is the features the project turned off, each with why.
	Off map[string]string
	// Harness is the harness's own features. They, core, and every feature
	// they require block readiness, and none of them is turned off unless
	// the box's own copy is usable (ADR 26-10-16-423 §4).
	Harness []string
	// Background provisions what readiness does not wait for after it is
	// reached, in parallel. Unset, every step blocks, as an image build or a
	// snapshot provisions (§8).
	Background bool
	// Snapshot is a run taking a snapshot (§8): the steps a feature leaves
	// out of one (provisionspec.Spec.InSnapshot) are Deferred, with every
	// step after them and every feature requiring theirs, so nothing runs
	// before what it was written to follow.
	Snapshot bool
}

// featurePlan is what a run does with one feature.
type featurePlan struct {
	feature provisionspec.Feature
	// off is why the feature is off; empty is on.
	off string
	// blocking is a feature readiness waits for.
	blocking bool
	// whole is a blocking feature readiness waits for every step of, its
	// async marks overridden: one the harness requires, or that a blocking
	// feature requires, which needs all of it in place before it runs.
	whole bool
	// firstAsync is the index of the feature's first step marked async: in
	// a blocking feature that is not whole, it and every step after it run
	// in the background, so the steps still run in the order written.
	firstAsync int
	adopted    bool
	version    string
	// deferredFrom is, in a run taking a snapshot (Options.Snapshot), the
	// index of the feature's first step left out of it: it and every step
	// after it are Deferred, so what the box applies at its boot still runs
	// in the order written. It is 0 for a feature left out whole, or that
	// requires one with a step left out, and the step count when nothing is.
	deferredFrom int
	// changed is whether this run applied any of its steps.
	changed bool
	// failed is why the feature failed.
	failed error
	done   chan struct{}
	out    io.Writer
	logs   *os.File
}

// blocks reports whether the feature's step at index waits for readiness.
func (p *featurePlan) blocks(index int, opts Options) bool {
	return !opts.Background || p.whole || (p.blocking && index < p.firstAsync)
}

// blockingSteps selects the steps past wiring that readiness waits for.
func (p *featurePlan) blockingSteps(opts Options) func(int, provisionspec.Step) bool {
	return func(i int, step provisionspec.Step) bool { return !step.Wiring() && p.blocks(i, opts) }
}

// backgroundSteps selects the steps left for after readiness.
func (p *featurePlan) backgroundSteps(opts Options) func(int, provisionspec.Step) bool {
	return func(i int, step provisionspec.Step) bool { return !step.Wiring() && !p.blocks(i, opts) }
}

// has reports whether any of the feature's steps is one want selects.
func (p *featurePlan) has(want func(int, provisionspec.Step) bool) bool {
	for i, step := range p.feature.Spec.Steps {
		if want(i, step) {
			return true
		}
	}
	return false
}

// Provision provisions features onto the box (ADR 26-10-16-423 §§4–5). Every
// feature that is on gets its wiring first — groups and env, and placeholders
// for the commands of those provisioned in the background. Then the steps
// readiness waits for: core's, the harness's, and every feature they require.
// With Background set, readiness is reported to the Tracker there, and the
// rest is provisioned in parallel, each feature after those it requires.
//
// A failed feature fails itself and the features requiring it, and every
// failure is returned. It returns once nothing is left to provision.
func (r *Runner) Provision(ctx context.Context, features []provisionspec.Feature, opts Options) ([]Outcome, error) {
	plans, err := r.begin(ctx, features, opts)
	if err != nil {
		return nil, err
	}
	defer closeLogs(plans)
	tracker := r.status
	byName := map[string]*featurePlan{}
	for _, plan := range plans {
		byName[plan.feature.Name] = plan
	}
	var (
		mu       sync.Mutex
		outcomes []Outcome
		errs     []error
	)
	record := func(applied []Outcome, err error) {
		mu.Lock()
		defer mu.Unlock()
		outcomes = append(outcomes, applied...)
		if err != nil {
			errs = append(errs, err)
		}
	}

	var blockingErrs []error
	if err := r.wire(ctx, plans, opts, record); err != nil {
		blockingErrs = append(blockingErrs, err)
	}

	// What readiness waits for, in requires order.
	for _, plan := range plans {
		if plan.off != "" || plan.failed != nil || !plan.has(plan.blockingSteps(opts)) {
			continue
		}
		if !r.requiredFailed(plan, byName) {
			record(r.runSteps(ctx, plan, plan.blockingSteps(opts)))
		}
		if plan.failed != nil {
			r.finish(plan)
		}
	}
	// A feature with nothing left for the background, whose requirements are
	// done, is done now, so provision.json says so from readiness on. In
	// requires order, a feature's requirements were decided before it.
	for _, plan := range plans {
		if plan.off != "" || plan.failed != nil || plan.has(plan.backgroundSteps(opts)) {
			continue
		}
		if slices.ContainsFunc(plan.feature.Spec.Requires, func(name string) bool { return !isDone(byName[name]) }) {
			continue
		}
		r.requiredFailed(plan, byName)
		r.finish(plan)
	}
	// Every failure so far was wiring's or a blocking step's. Readiness's
	// error is what a person attaching sees, so it names each one's log.
	for _, plan := range plans {
		if plan.failed == nil {
			continue
		}
		if path := r.logPath(plan.feature.Name); path != "" && plan.logs != nil {
			blockingErrs = append(blockingErrs, fmt.Errorf("%w (log: %s)", plan.failed, path))
		} else {
			blockingErrs = append(blockingErrs, plan.failed)
		}
	}
	tracker.ready(errors.Join(blockingErrs...))

	// The rest, in parallel, each feature once those it requires are done.
	var wg sync.WaitGroup
	for _, plan := range plans {
		select {
		case <-plan.done:
			continue
		default:
		}
		if plan.off != "" {
			continue
		}
		wg.Go(func() {
			for _, name := range plan.feature.Spec.Requires {
				<-byName[name].done
			}
			if !r.requiredFailed(plan, byName) {
				record(r.runSteps(ctx, plan, plan.backgroundSteps(opts)))
			}
			r.finish(plan)
		})
	}
	wg.Wait()
	for _, plan := range plans {
		if plan.failed != nil && !slices.ContainsFunc(errs, func(err error) bool { return errors.Is(err, plan.failed) }) {
			errs = append(errs, plan.failed)
		}
	}
	tracker.finished()
	return outcomes, errors.Join(errs...)
}

// Wire is Provision's wiring alone: what is decided of every feature, each
// one's groups and env, and the placeholders (ADR 26-10-16-423 §5). A box's
// boot runs it before the sandbox's account exists, so the account joins
// groups already there; the agent's Provision then finds it in place. Its
// failures are reported to the Tracker and returned, and readiness is left
// for Provision to decide.
func (r *Runner) Wire(ctx context.Context, features []provisionspec.Feature, opts Options) error {
	plans, err := r.begin(ctx, features, opts)
	if err != nil {
		return err
	}
	defer closeLogs(plans)
	var errs []error
	if err := r.wire(ctx, plans, opts, func(_ []Outcome, err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// begin plans a run and reports the plan; a plan refused fails readiness.
func (r *Runner) begin(ctx context.Context, features []provisionspec.Feature, opts Options) ([]*featurePlan, error) {
	r.status = r.Tracker
	if r.status == nil {
		r.status = NewTracker("")
	}
	plans, err := r.plan(ctx, features, opts)
	if err != nil {
		r.status.Fail(err)
		return nil, err
	}
	r.status.start(plans)
	return plans, nil
}

// wire applies every feature's wiring, for each that is on, and writes the
// placeholders. It is what goes before the account exists, so a group joined
// later reaches no running session.
func (r *Runner) wire(ctx context.Context, plans []*featurePlan, opts Options, record func([]Outcome, error)) error {
	for _, plan := range plans {
		if plan.off != "" {
			continue
		}
		record(r.runSteps(ctx, plan, func(_ int, step provisionspec.Step) bool { return step.Wiring() }))
		if plan.failed == nil {
			plan.failed = r.pruneEnvProfiles(plan.feature, false)
		}
		if plan.failed != nil {
			r.finish(plan)
		}
	}
	return r.writePlaceholders(plans, opts)
}

// isDone reports whether a feature is done this run.
func isDone(plan *featurePlan) bool {
	select {
	case <-plan.done:
		return true
	default:
		return false
	}
}

func closeLogs(plans []*featurePlan) {
	for _, plan := range plans {
		if plan.logs != nil {
			plan.logs.Close()
		}
	}
}

// requiredFailed fails plan when a feature it requires failed, and reports
// whether it did.
func (r *Runner) requiredFailed(plan *featurePlan, plans map[string]*featurePlan) bool {
	for _, name := range plan.feature.Spec.Requires {
		if plans[name].failed != nil {
			plan.failed = fmt.Errorf("feature %q: not provisioned: it requires %q, which failed", plan.feature.Name, name)
			return true
		}
	}
	return false
}

// finish marks a feature done: ready unless it failed.
func (r *Runner) finish(plan *featurePlan) {
	select {
	case <-plan.done:
		return
	default:
	}
	if plan.failed == nil && plan.feature.Spec.Commands != nil {
		if err := r.removePlaceholders(plan); err != nil {
			r.logger().Warn("provision: remove placeholders", "feature", plan.feature.Name, "error", err)
		}
	}
	r.tracker().finish(plan)
	close(plan.done)
}

// runSteps applies the feature's steps that want selects, in order, stopping
// at the first that fails.
func (r *Runner) runSteps(ctx context.Context, plan *featurePlan, want func(int, provisionspec.Step) bool) ([]Outcome, error) {
	feature := plan.feature
	var outcomes []Outcome
	for i, step := range feature.Spec.Steps {
		if !want(i, step) {
			continue
		}
		if step.Install && plan.adopted {
			outcomes = append(outcomes, Outcome{Feature: feature.Name, Step: step.Name, Result: NotInstalled})
			continue
		}
		if i >= plan.deferredFrom {
			outcomes = append(outcomes, Outcome{Feature: feature.Name, Step: step.Name, Result: Deferred})
			continue
		}
		out, err := r.output(plan)
		if err != nil {
			plan.failed = fmt.Errorf("feature %q: %w", feature.Name, err)
			return outcomes, plan.failed
		}
		r.tracker().step(feature.Name, step.Name)
		result, err := r.applyStep(ctx, out, feature, step)
		if err != nil {
			plan.failed = fmt.Errorf("feature %q: step %q: %w", feature.Name, step.Name, err)
			fmt.Fprintf(out, "discobox provision: %s: step %s failed: %v\n", feature.Name, step.Name, err)
			return outcomes, plan.failed
		}
		if result == Applied || result == Reapplied {
			plan.changed = true
		}
		r.logger().Info("provision", "feature", feature.Name, "step", step.Name, "kind", step.Kind(), "result", result)
		outcomes = append(outcomes, Outcome{Feature: feature.Name, Step: step.Name, Result: result})
	}
	r.tracker().step(feature.Name, "")
	return outcomes, nil
}

// output is where a feature's steps write: its log, opened on first use.
func (r *Runner) output(plan *featurePlan) (io.Writer, error) {
	if plan.out != nil {
		return plan.out, nil
	}
	if r.LogDir == "" {
		plan.out = os.Stderr
		return plan.out, nil
	}
	path := r.path(r.logPath(plan.feature.Name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	logs, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	plan.logs, plan.out = logs, logs
	r.tracker().logPath(plan.feature.Name, r.logPath(plan.feature.Name))
	return plan.out, nil
}

// logPath is a feature's log on the box, or "" when steps write to stderr.
func (r *Runner) logPath(feature string) string {
	if r.LogDir == "" {
		return ""
	}
	return filepath.Join(r.LogDir, feature+".log")
}

// plan decides what becomes of each feature: off, adopted or installed, and
// whether readiness waits for it. A feature the harness or core requires that
// the project turned off is refused, naming both, unless the box's own copy
// is usable, which satisfies it.
func (r *Runner) plan(ctx context.Context, features []provisionspec.Feature, opts Options) ([]*featurePlan, error) {
	// A box with nothing to provision is any box: one whose image brought
	// its tools is never refused for its distribution.
	if len(features) == 0 {
		return nil, nil
	}
	if err := r.Platform.Supported(); err != nil {
		return nil, err
	}
	ordered, err := order(features)
	if err != nil {
		return nil, err
	}
	byName := map[string]provisionspec.Feature{}
	for _, feature := range ordered {
		byName[feature.Name] = feature
	}
	for _, name := range opts.Harness {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("the harness's feature %q is not there", name)
		}
	}
	// requiredBy is, for each feature core or the harness needs, what needs
	// it, to name in a refusal.
	requiredBy := map[string]string{}
	harness := map[string]bool{}
	var require func(name, by string, fromHarness bool)
	require = func(name, by string, fromHarness bool) {
		if _, ok := requiredBy[name]; ok && (harness[name] || !fromHarness) {
			return
		}
		requiredBy[name] = by
		if fromHarness {
			harness[name] = true
		}
		for _, next := range byName[name].Spec.Requires {
			require(next, by, fromHarness)
		}
	}
	for _, name := range opts.Harness {
		require(name, fmt.Sprintf("the harness's feature %q", name), true)
	}
	core, hasCore := byName[provisionspec.Core]
	if hasCore {
		require(provisionspec.Core, "core", false)
	}
	// whole is every feature a blocking feature requires: the harness's
	// closure, and core's requirements unless core itself is async.
	whole := maps.Clone(harness)
	var wholly func(name string)
	wholly = func(name string) {
		if whole[name] {
			return
		}
		whole[name] = true
		for _, next := range byName[name].Spec.Requires {
			wholly(next)
		}
	}
	if hasCore && (harness[provisionspec.Core] || !core.Spec.Async) {
		for _, name := range core.Spec.Requires {
			wholly(name)
		}
	}

	var plans []*featurePlan
	byPlan := map[string]*featurePlan{}
	var refused []error
	for _, feature := range ordered {
		plan := &featurePlan{feature: feature, deferredFrom: len(feature.Spec.Steps), done: make(chan struct{})}
		plans = append(plans, plan)
		byPlan[feature.Name] = plan
		_, required := requiredBy[feature.Name]
		plan.whole = whole[feature.Name]
		plan.blocking = plan.whole || (feature.Name == provisionspec.Core && !feature.Spec.Async)
		plan.firstAsync = len(feature.Spec.Steps)
		if i := slices.IndexFunc(feature.Spec.Steps, func(step provisionspec.Step) bool { return step.Async }); i >= 0 {
			plan.firstAsync = i
		}
		if opts.Snapshot {
			if i := slices.IndexFunc(feature.Spec.Steps, func(step provisionspec.Step) bool { return !feature.Spec.InSnapshot(step) }); i >= 0 {
				plan.deferredFrom = i
			}
			// In requires order, what it requires is planned already.
			if slices.ContainsFunc(feature.Spec.Requires, func(name string) bool {
				required := byPlan[name]
				return required.deferredFrom < len(required.feature.Spec.Steps)
			}) {
				plan.deferredFrom = 0
			}
		}
		if feature.Spec.Adopt != nil && !r.installedHere(feature) {
			version, err := r.usable(ctx, feature)
			if err != nil {
				r.logger().Info("provision: not adopted", "feature", feature.Name, "reason", err)
			} else {
				plan.adopted, plan.version = true, version
			}
		}
		reason, optedOut := opts.Off[feature.Name]
		switch {
		case optedOut && required && feature.Name == provisionspec.Core:
			refused = append(refused, errors.New("core is not a feature and cannot be turned off"))
		case optedOut && required && !plan.adopted:
			refused = append(refused, fmt.Errorf("%s requires %q, which is turned off (%s), and the box has no usable %s of its own", requiredBy[feature.Name], feature.Name, reason, feature.Name))
		case optedOut && !required:
			plan.off = reason
		default:
			for _, name := range feature.Spec.Requires {
				if byPlan[name].off != "" && !required {
					plan.off = fmt.Sprintf("it requires %q, which is off", name)
					break
				}
			}
		}
	}
	if err := errors.Join(refused...); err != nil {
		return nil, err
	}
	for _, plan := range plans {
		if plan.off != "" {
			// Opting out never uninstalls: it skips the feature's wiring,
			// so the env it put on every login shell goes.
			if err := r.pruneEnvProfiles(plan.feature, true); err != nil {
				return nil, fmt.Errorf("feature %q: %w", plan.feature.Name, err)
			}
			close(plan.done)
		}
	}
	return plans, nil
}

// installedHere reports whether the box has a record of one of the feature's
// install steps: then the copy it has is ours, and is not adopted.
func (r *Runner) installedHere(feature provisionspec.Feature) bool {
	for _, step := range feature.Spec.Steps {
		if !step.Install {
			continue
		}
		if _, ok, err := r.Records.Step(feature.Name, step.Name); err == nil && ok {
			return true
		}
	}
	return false
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

func (r *Runner) applyStep(ctx context.Context, out io.Writer, feature provisionspec.Feature, step provisionspec.Step) (Result, error) {
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
		inPlace, err := r.check(ctx, out, feature, step.Check)
		if err != nil {
			return "", err
		}
		if inPlace {
			return Skipped, nil
		}
		result = Reapplied
	}
	if err := r.apply(ctx, out, feature, step); err != nil {
		return "", err
	}
	if err := r.Records.RecordStep(feature.Name, step.Name, StepRecord{Digest: digest, AppliedAt: r.now()}); err != nil {
		return "", fmt.Errorf("record: %w", err)
	}
	return result, nil
}

// check runs a check script: exit 0 is in place, any other exit is drift, and
// a script that could not be run at all is an error.
func (r *Runner) check(ctx context.Context, out io.Writer, feature provisionspec.Feature, script string) (bool, error) {
	err := r.script(ctx, out, feature, script)
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

func (r *Runner) apply(ctx context.Context, out io.Writer, feature provisionspec.Feature, step provisionspec.Step) error {
	switch {
	case step.File != nil:
		return r.applyFile(feature, step.File)
	case step.Systemd != nil:
		return r.applySystemd(ctx, out, feature, step.Systemd)
	case step.Group != nil:
		return r.applyGroup(ctx, out, step.Group)
	case step.Env != nil:
		return r.applyEnv(feature, step)
	case step.Download != nil:
		return r.applyDownload(ctx, step.Download)
	case step.Packages != nil:
		return r.applyPackages(ctx, out, step.Packages)
	case step.Run != "":
		if step.Apt {
			r.apt.Lock()
			defer r.apt.Unlock()
		}
		return r.script(ctx, out, feature, step.Run)
	}
	return errors.New("names no kind")
}

// script runs one of the feature's scripts from the feature's directory.
func (r *Runner) script(ctx context.Context, out io.Writer, feature provisionspec.Feature, script string) error {
	env := []string{
		"DISCOBOX_PROVISION_FEATURE=" + feature.Name,
		"DISCOBOX_PROVISION_FEATURE_DIR=" + feature.Dir,
	}
	return r.command(ctx, out, feature.Dir, env, filepath.Join(feature.Dir, filepath.FromSlash(script)))
}

func (r *Runner) command(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error {
	if r.Command != nil {
		return r.Command(ctx, out, dir, env, name, args...)
	}
	return runCommand(ctx, out, dir, env, name, args...)
}

func runCommand(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = out
	cmd.Stderr = out
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

func (r *Runner) tracker() *Tracker {
	return r.status
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
