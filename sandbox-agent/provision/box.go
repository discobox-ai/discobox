package provision

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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

// Apply provisions the contents with the supplied execution policy. Feature
// opt-outs are applied for every delivery source. wireOnly runs account wiring
// before the account exists; Background and Snapshot retain their runner meaning.
func Apply(ctx context.Context, logger *slog.Logger, tracker *Tracker, trees []string, features map[string]sandboxconfig.Feature, options Options, wireOnly bool) error {
	loaded, opts, err := BootPlan(trees, features)
	if err != nil {
		tracker.Fail(err)
		return err
	}
	opts.Background, opts.Snapshot, opts.Retry = options.Background, options.Snapshot, options.Retry
	off := maps.Clone(options.Off)
	if off == nil {
		off = map[string]string{}
	}
	for name, reason := range opts.Off {
		off[name] = reason
	}
	opts.Off = off
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
	// A reprovision runs the boot's plan again, which is already published.
	if runner.Booted && !opts.Retry {
		if err := publishConfigurationPlan(loaded, opts); err != nil {
			tracker.Fail(err)
			return err
		}
	}
	_, err = runner.Provision(ctx, loaded, opts)
	return err
}

// booted reports whether systemd is running this box: a box's boot, never
// an image's build.
func booted() bool { return dirExists("/run/systemd/system") }
