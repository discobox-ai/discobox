package provision

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// LogDir holds each feature's log at boot.
const LogDir = "/var/log/discobox/provision"

// optedOutReason is why a feature the project turned off is off.
const optedOutReason = "turned off in " + sandboxconfig.SettingsFile

// BootPlan is what a box provisions at boot: the features of its bundle
// trees, in the order its pins name them, and the options the bootstrap's
// features make of them. A box with no trees has no features, and its tools
// are what its image came with.
func BootPlan(trees []string, features map[string]sandboxconfig.Feature) ([]provisionspec.Feature, Options, error) {
	opts := Options{Off: map[string]string{}, Background: true}
	for name, feature := range features {
		if !feature.Enabled {
			opts.Off[name] = optedOutReason
		}
	}
	if len(trees) == 0 {
		return nil, opts, nil
	}
	loaded, err := provisionspec.Load(trees...)
	if err != nil {
		return nil, opts, fmt.Errorf("load bundle trees: %w", err)
	}
	return loaded, opts, nil
}

// Boot provisions this box at boot from its bundle trees, in order, turning
// off the features the bootstrap does, and reports to tracker, which writes
// provision.json. wireOnly runs the wiring alone (Runner.Wire), as boot does
// before the account exists; otherwise it is the whole run (Runner.Provision),
// the agent's, which returns once nothing is left to provision.
func Boot(ctx context.Context, logger *slog.Logger, tracker *Tracker, trees []string, features map[string]sandboxconfig.Feature, wireOnly bool) error {
	loaded, opts, err := BootPlan(trees, features)
	if err != nil {
		tracker.Fail(err)
		return err
	}
	runner := &Runner{
		Root:         "/",
		Records:      Records{Dir: RecordsDir},
		LogDir:       LogDir,
		Placeholders: PlaceholderDir,
		Tracker:      tracker,
		Logger:       logger,
		Booted:       booted(),
	}
	if self, err := os.Executable(); err == nil {
		runner.Self = self
	}
	if len(loaded) > 0 {
		if runner.Platform, err = DetectPlatform("/"); err != nil {
			tracker.Fail(err)
			return err
		}
	}
	if wireOnly {
		return runner.Wire(ctx, loaded, opts)
	}
	_, err = runner.Provision(ctx, loaded, opts)
	return err
}

// Snapshot provisions this box from its bundle trees, in order, as a
// snapshot is taken
// (ADR 26-10-16-423 §8): the features turned off as a sandbox's bootstrap
// turns them off, every step blocking, and the steps a feature keeps out of a
// snapshot left for the boot of each box made from it (Options.Snapshot). It
// reports to tracker, and returns once nothing is left to provision.
func Snapshot(ctx context.Context, logger *slog.Logger, tracker *Tracker, trees []string, features map[string]sandboxconfig.Feature) error {
	loaded, opts, err := BootPlan(trees, features)
	if err != nil {
		tracker.Fail(err)
		return err
	}
	opts.Background, opts.Snapshot = false, true
	runner := &Runner{
		Root:    "/",
		Records: Records{Dir: RecordsDir},
		LogDir:  LogDir,
		Tracker: tracker,
		Logger:  logger,
		Booted:  booted(),
	}
	if self, err := os.Executable(); err == nil {
		runner.Self = self
	}
	if runner.Platform, err = DetectPlatform("/"); err != nil {
		tracker.Fail(err)
		return err
	}
	_, err = runner.Provision(ctx, loaded, opts)
	return err
}

// booted reports whether systemd is running this box: a box's boot, never
// an image's build.
func booted() bool { return dirExists("/run/systemd/system") }
