package provision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// The bundle trees a box provisions at boot: the platform's, and the
// harness's, whose features and every feature they require block readiness.
// Each is provisioned when it is there; a box with neither has no features,
// and its tools are what its image came with.
const (
	PlatformTree = "/opt/discobox/bundles/platform"
	HarnessTree  = "/opt/discobox/bundles/harness"
)

// LogDir holds each feature's log at boot.
const LogDir = "/var/log/discobox/provision"

// optedOutReason is why a feature the project turned off is off.
const optedOutReason = "turned off in " + sandboxconfig.SettingsFile

// BootPlan is what a box provisions at boot: the features of the bundle
// trees under root, and the options the bootstrap's features and the harness
// tree make of them.
func BootPlan(root string, features map[string]sandboxconfig.Feature) ([]provisionspec.Feature, Options, error) {
	opts := Options{Off: map[string]string{}, Background: true}
	for name, feature := range features {
		if !feature.Enabled {
			opts.Off[name] = optedOutReason
		}
	}
	var trees []string
	for _, tree := range []string{PlatformTree, HarnessTree} {
		dir := filepath.Join(root, tree)
		if _, err := os.Stat(filepath.Join(dir, provisionspec.FeaturesDir)); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, opts, err
		}
		trees = append(trees, dir)
		if tree == HarnessTree {
			harness, err := provisionspec.Load(dir)
			if err != nil {
				return nil, opts, err
			}
			for _, feature := range harness {
				opts.Harness = append(opts.Harness, feature.Name)
			}
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

// Boot provisions this box at boot from its bundle trees, turning off the
// features the bootstrap does, and reports to tracker, which writes
// provision.json. wireOnly runs the wiring alone (Runner.Wire), as boot does
// before the account exists; otherwise it is the whole run (Runner.Provision),
// the agent's, which returns once nothing is left to provision.
func Boot(ctx context.Context, logger *slog.Logger, tracker *Tracker, features map[string]sandboxconfig.Feature, wireOnly bool) error {
	loaded, opts, err := BootPlan("/", features)
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

// Trees provisions every feature of the bundle trees onto this box,
// recording each step under records, with every step blocking and none
// turned off: what an image build or a snapshot provisions (ADR 26-10-16-423
// §8). self is the sandbox agent's executable when the agent is what runs
// it (Runner.Self), and empty otherwise. A box's boot provisions with Boot
// instead.
func Trees(ctx context.Context, logger *slog.Logger, records, self string, trees ...string) error {
	features, err := provisionspec.Load(trees...)
	if err != nil {
		return err
	}
	runner := &Runner{Root: "/", Records: Records{Dir: records}, Self: self, Logger: logger}
	if runner.Platform, err = DetectPlatform("/"); err != nil {
		return err
	}
	_, err = runner.Provision(ctx, features, Options{})
	return err
}
