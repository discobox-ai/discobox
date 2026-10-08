package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// proxyUnit is a unit that reads the proxy material, and what it reads.
type proxyUnit struct {
	name string
	// config is the file whose presence the unit is conditioned on
	// (ConditionPathExists): with it the unit runs, without it the unit stops.
	config string
	// inputs are the files the unit reads when it starts, config included.
	inputs []string
	// withDocker marks the nested-Docker bridge, which runs only while dockerd
	// does (docker.service Upholds= it): it is restarted when dockerd is up and
	// otherwise left for dockerd to bring up.
	withDocker bool
	// listen is the address a started unit serves on. The bridges are
	// Type=simple, so their start job ends when the process is forked; what
	// says one is up is that its address accepts a connection.
	listen string
}

// proxyUnits are the sandbox's units that read the proxy material, in the
// order they are started: the trust store first, so a bridge that has just
// come up is not used by a client that does not yet trust what it intercepts.
var proxyUnits = []proxyUnit{
	{name: "discobox-trust-ca.service", config: mitmCAFile, inputs: []string{mitmCAFile}},
	{name: "discobox-proxy-bridge.service", config: egressBridgeFile, inputs: bridgeInputs(egressBridgeFile), listen: sandboxconfig.SandboxEgressListenAddress},
	{name: "discobox-buildkit-bridge.service", config: buildKitBridgeFile, inputs: bridgeInputs(buildKitBridgeFile), listen: sandboxconfig.SandboxBuildKitListenAddress},
	{name: "discobox-proxy-bridge-docker.service", config: nestedDockerBridgeFile, inputs: bridgeInputs(nestedDockerBridgeFile), withDocker: true},
}

func bridgeInputs(config string) []string {
	return []string{config, mtlsCAFile, clientCertFile, clientKeyFile}
}

// unitAction is what a delivery asks of one unit.
type unitAction struct {
	unit proxyUnit
	// stop is set when the unit's config is gone; otherwise it is restarted.
	stop bool
}

// unitActions are the units an applied delivery affects — those reading a
// file in changed — and whether each is restarted or stopped, decided by
// whether its config is in proxyDir now.
func unitActions(proxyDir string, changed []string) []unitAction {
	touched := map[string]bool{}
	for _, path := range changed {
		if filepath.Dir(path) == filepath.Clean(proxyDir) {
			touched[filepath.Base(path)] = true
		}
	}
	var out []unitAction
	for _, unit := range proxyUnits {
		affected := false
		for _, input := range unit.inputs {
			affected = affected || touched[input]
		}
		if !affected {
			continue
		}
		_, err := os.Stat(filepath.Join(proxyDir, unit.config))
		out = append(out, unitAction{unit: unit, stop: err != nil})
	}
	return out
}

// UnitActivator starts, restarts or stops the units that read the proxy
// material a delivery changed (ADR 26-10-08-127 §5). They read it when they
// start and are conditioned on it being there, so material that arrives after
// boot reaches them only this way. A started unit counts once it is up — a
// bridge once its address accepts connections — and any unit that is not is an
// error, which keeps the delivery from publishing readiness over a hop that is
// not there; the next delivery tries it again.
func UnitActivator(proxyDir string, logger *slog.Logger) Activator {
	if logger == nil {
		logger = slog.Default()
	}
	return func(ctx context.Context, changed []string) error {
		var errs []error
		for _, action := range unitActions(proxyDir, changed) {
			if err := applyUnitAction(ctx, action); err != nil {
				logger.Warn("start a unit for the delivered runtime config", "unit", action.unit.name, "stop", action.stop, "error", err)
				errs = append(errs, fmt.Errorf("%s: %w", action.unit.name, err))
			}
		}
		return errors.Join(errs...)
	}
}
