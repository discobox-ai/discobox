package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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

const bundleRetryMaxWait = 30 * time.Second

var bundleRetryWait = 2 * time.Second

// provisionBox provisions the box at its start (ADR 26-10-16-423 §§5, 7). A
// box whose pins its pool placed with the agent first brings its bundles into
// its content store — once its first runtime-config document is applied,
// since that brings up the bridge every fetch goes through — and provisions
// the trees they make. One with no pins, whose agent is not delivered to it —
// a template with no image, on its own platform — provisions the trees it
// has, if any.
//
// retry is a reprovision's run (ADR 26-11-11-560 §2): the box's bundles are
// in its store already, and only the features not ready run.
func provisionBox(ctx context.Context, tracker *provision.Tracker, runtimeConfig *intake.Intake, configPath string, features map[string]sandboxconfig.Feature, retry bool) error {
	if runtime.GOOS == "linux" && !retry {
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
	trees, err := bundlestore.Trees("/")
	if err != nil {
		tracker.Fail(err)
		return err
	}
	return provision.Apply(ctx, slog.Default(), tracker, trees, features, provision.Options{Background: true, Retry: retry}, false)
}

// fetchBundles applies the pins in agentlayer.PinsFile, when the pool placed
// one, to the box's content store, and reports whether it did. It waits for
// the box's first runtime-config document, which brings up the bridge every
// fetch from assets.discobox.internal goes through.
func fetchBundles(ctx context.Context, runtimeConfig *intake.Intake) (bool, error) {
	pins, ok, err := placedPins()
	if err != nil || !ok {
		return false, err
	}
	if _, err := runtimeConfig.WaitApplied(ctx); err != nil {
		return false, err
	}
	store := &bundlestore.Store{Root: "/", Fetcher: bundlestore.HTTPFetcher{}, Platform: imagecache.PoolPlatform(), Logger: slog.Default()}
	return true, applyBundles(ctx, pins, store)
}

// placedPins is agentlayer.PinsFile, which the box's maker placed, and false
// when it placed none.
func placedPins() (agentlayer.Pins, bool, error) {
	data, err := os.ReadFile(agentlayer.PinsFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	pins, err := agentlayer.ParsePins(data)
	return pins, err == nil, err
}

// applyBundles brings pins into the box's content store from
// assets.discobox.internal. Transient pool outages wait until recovery or
// cancellation; invalid pins, content and local failures fail immediately.
func applyBundles(ctx context.Context, pins agentlayer.Pins, store *bundlestore.Store) error {
	wait := bundleRetryWait
	for attempt := 1; ; attempt++ {
		err := store.Apply(ctx, pins)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !bundlestore.Retryable(err) {
			return fmt.Errorf("fetch the box's bundles: %w", err)
		}
		slog.Warn("fetch the box's bundles; trying again", "attempt", attempt, "in", wait, "error", err)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
		wait = min(wait*2, bundleRetryMaxWait)
	}
}
