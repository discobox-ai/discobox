package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/intake"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// reprovisioner runs a running box's failed features again, in this
// process, on the tracker its boot's run reported to (ADR 26-11-11-560 §1):
// the same trees, feature settings and step records, and nothing about the
// box changed. The API's reprovision route and the in-box requests
// (provision.RetryDir) both come here.
type reprovisioner struct {
	// ctx is the agent's: a run outlives the request that started it.
	ctx           context.Context
	tracker       *provision.Tracker
	runtimeConfig *intake.Intake
	configPath    string
	features      map[string]sandboxconfig.Feature
}

// Reprovision starts a run, or joins the one in progress, and is the
// document as it then stands (ADR 26-11-11-560 §§2–3).
func (p *reprovisioner) Reprovision() provisionspec.Status {
	status, started := p.tracker.Rerun()
	if started {
		go p.run(!hasPending(status))
	}
	return status
}

// run is the run Reprovision started. A run that failed with no feature to
// name runs the boot's path whole, its bundles fetched again; any other runs
// the features that are not ready, on the trees in place.
func (p *reprovisioner) run(whole bool) {
	err := provisionBox(p.ctx, p.tracker, p.runtimeConfig, p.configPath, p.features, !whole)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("reprovision", "error", err)
	}
}

// hasPending reports whether a feature is pending: what a started rerun
// made of each failed feature.
func hasPending(status provisionspec.Status) bool {
	for _, feature := range status.Features {
		if feature.State == provisionspec.StatePending {
			return true
		}
	}
	return false
}
