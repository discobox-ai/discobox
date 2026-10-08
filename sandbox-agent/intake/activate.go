package intake

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

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
	// reloads are the inputs a running unit reads again itself whenever they
	// change — a bridge's mTLS material, which it takes afresh for every
	// handshake (bridge.Material). A delivery that changes only these does not
	// restart the unit: it is started if it is not running, and otherwise left
	// alone, so the connections it carries survive a certificate renewal.
	reloads []string
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
	{name: "discobox-proxy-bridge.service", config: egressBridgeFile, inputs: bridgeInputs(egressBridgeFile), reloads: bridgeMaterial, listen: sandboxconfig.SandboxEgressListenAddress},
	{name: "discobox-buildkit-bridge.service", config: buildKitBridgeFile, inputs: bridgeInputs(buildKitBridgeFile), reloads: bridgeMaterial, listen: sandboxconfig.SandboxBuildKitListenAddress},
	{name: "discobox-proxy-bridge-docker.service", config: nestedDockerBridgeFile, inputs: bridgeInputs(nestedDockerBridgeFile), reloads: bridgeMaterial, withDocker: true},
}

// bridgeMaterial is the mTLS material every bridge reads, and reads again
// whenever it changes.
var bridgeMaterial = []string{mtlsCAFile, clientCertFile, clientKeyFile}

func bridgeInputs(config string) []string {
	return append([]string{config}, bridgeMaterial...)
}

// unitVerb is what a delivery asks systemd to do with one unit.
type unitVerb string

const (
	// verbRestart restarts a unit that must read its inputs again.
	verbRestart unitVerb = "restart"
	// verbStart starts a unit that is not running and leaves a running one be:
	// it has already taken the change itself.
	verbStart unitVerb = "start"
	// verbStop stops a unit whose config is gone.
	verbStop unitVerb = "stop"
)

// unitAction is what a delivery asks of one unit.
type unitAction struct {
	unit proxyUnit
	verb unitVerb
}

// unitActions are the units an applied delivery affects — those reading a
// file in changed — and what each is asked: stopped when its config is no
// longer in proxyDir, started when every changed input is one it reloads
// itself, and otherwise restarted.
func unitActions(proxyDir string, changed []string) []unitAction {
	touched := map[string]bool{}
	for _, path := range changed {
		if filepath.Dir(path) == filepath.Clean(proxyDir) {
			touched[filepath.Base(path)] = true
		}
	}
	var out []unitAction
	for _, unit := range proxyUnits {
		affected, reloaded := false, true
		for _, input := range unit.inputs {
			if touched[input] {
				affected = true
				reloaded = reloaded && slices.Contains(unit.reloads, input)
			}
		}
		if !affected {
			continue
		}
		verb := verbRestart
		if _, err := os.Stat(filepath.Join(proxyDir, unit.config)); err != nil {
			verb = verbStop
		} else if reloaded {
			verb = verbStart
		}
		out = append(out, unitAction{unit: unit, verb: verb})
	}
	return out
}

// UnitActivator starts, restarts or stops the units that read the proxy
// material a delivery changed (ADR 26-10-08-127 §5). They read it when they
// start and are conditioned on it being there, so material that arrives after
// boot reaches them only this way — except a renewed keypair or CA, which a
// running bridge takes itself, so a renewal restarts nothing. A started unit counts once it is up — a
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
				logger.Warn("start a unit for the delivered runtime config", "unit", action.unit.name, "verb", action.verb, "error", err)
				errs = append(errs, fmt.Errorf("%s: %w", action.unit.name, err))
			}
		}
		return errors.Join(errs...)
	}
}
