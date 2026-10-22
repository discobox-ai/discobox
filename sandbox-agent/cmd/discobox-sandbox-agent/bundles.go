package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/boot"
	"github.com/discobox-ai/discobox/sandbox-agent/bundlestore"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
	"github.com/discobox-ai/discobox/sandbox-agent/proxyenv"
)

// exportProxyEnv takes the bootstrap's proxy-trust env into the agent's own,
// a variable already set left as it is.
func exportProxyEnv(configPath string) error {
	vars, err := proxyenv.Vars(configPath)
	if err != nil {
		return err
	}
	for name, value := range vars {
		if _, set := os.LookupEnv(name); set {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return err
		}
	}
	return nil
}

// bundleAttempts bounds how often a box tries its bundles before its
// readiness fails, each attempt after twice the wait of the one before.
const bundleAttempts = 5

var bundleRetryWait = 2 * time.Second

// provisionBox provisions the box at its start (ADR 26-10-16-423 §§5, 7). A
// box whose pins its pool placed with the agent first brings its bundles into
// its content store — once its first runtime-config document is applied,
// since that brings up the bridge every fetch goes through — and provisions
// the trees they make; one without provisions the trees it has.
func provisionBox(ctx context.Context, tracker *provision.Tracker, runtimeConfig *intake.Intake, configPath string, features map[string]sandboxconfig.Feature) error {
	if runtime.GOOS == "linux" {
		fetched, err := fetchBundles(ctx, runtimeConfig)
		if err == nil && fetched {
			// The account was made before the trees were here.
			err = boot.JoinFeatureGroups(slog.Default(), configPath)
		}
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				tracker.Fail(err)
			}
			return err
		}
	}
	return provision.Boot(ctx, slog.Default(), tracker, features, false)
}

// fetchBundles applies the pins in agentlayer.PinsFile, when the pool placed
// one, to the box's content store, and reports whether it did.
func fetchBundles(ctx context.Context, runtimeConfig *intake.Intake) (bool, error) {
	data, err := os.ReadFile(agentlayer.PinsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pins, err := agentlayer.ParsePins(data)
	if err != nil {
		return false, err
	}
	if _, err := runtimeConfig.WaitApplied(ctx); err != nil {
		return false, err
	}
	store := &bundlestore.Store{Root: "/", Fetcher: bundlestore.HTTPFetcher{}, Platform: imagecache.PoolPlatform(), Logger: slog.Default()}
	wait := bundleRetryWait
	for attempt := 1; ; attempt++ {
		err := store.Apply(ctx, pins)
		if err == nil {
			return true, nil
		}
		if attempt == bundleAttempts || ctx.Err() != nil {
			return false, fmt.Errorf("fetch the box's bundles: %w", err)
		}
		slog.Warn("fetch the box's bundles; trying again", "attempt", attempt, "in", wait, "error", err)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false, ctx.Err()
		}
		wait *= 2
	}
}

// runInstall lays the agent's units (agentlayer.Install), which run it from
// agentlayer.CurrentLink, and, on a box systemd is running, reloads systemd
// and starts the agent's bootstrap path unit, after which the agent
// provisions the box from its pins (ADR 26-10-16-423 §7). A delivered
// container never runs it: its pool lays the same units before it starts.
// The runnable image runs it at its build, where no systemd runs, so it lays
// the units alone; a machine's provider runs it once at the machine's first
// boot, having placed the agent layer and CurrentLink.
func runInstall(args []string) int {
	flags := flag.NewFlagSet("discobox-sandbox-agent install", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if err := agentlayer.Install("/"); err != nil {
		slog.Error("install the agent's units", "error", err)
		return 1
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return 0
	}
	for _, command := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "start", "discobox-sandbox-agent.path"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		//nolint:gosec // G204: the commands are the fixed list above.
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			slog.Error("start the agent", "command", command, "error", err)
			return 1
		}
	}
	return 0
}
