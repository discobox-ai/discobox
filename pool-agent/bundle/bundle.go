// Package bundle is the pool bundle (ADR 26-10-24-953 §2): the features under
// features/ that make a box a pool's, which pool-agent/Dockerfile's bundle
// stage builds FROM scratch, and where a pool's maker places what the pool
// agent reads beside them.
//
// A pool's maker places the agent bundle, the pool's pins and these files,
// and runs `discobox-provision apply`; the pool-agent feature lays and starts
// the agent's unit, which reads them.
package bundle

const (
	// BootstrapFile is the pool's poolagent.Bootstrap as JSON, root's alone,
	// which the agent's unit names to it (poolagent.EnvBootstrapFile).
	BootstrapFile = "/etc/discobox/pool-agent/bootstrap.json"
	// EnvironmentFile is the environment the maker sets the agent beside the
	// bootstrap: the pool's policy the agent reads from its environment.
	// Optional; the agent's unit reads it when it is there.
	EnvironmentFile = "/etc/discobox/pool-agent/environment"
	// ProvisioningKeyFile is a machine pool's provisioning key (ADR
	// 26-10-23-551 §4): a disco-vm machine.Key as JSON, root's alone, which
	// the pool's engine reads on every call to its driver, so a renewed key
	// is a new file and nothing more.
	ProvisioningKeyFile = "/etc/discobox/pool-agent/provisioning-key.json"
	// SandboxEdgeCredentialFile is the credential a machine pool's sandboxes
	// open its listener with, through the provider's edge (ADR 26-11-16-619
	// §2): a disco-vm machine.Credential as JSON, root's alone, which the
	// server mints and renews before it expires, and the agent reads as it
	// decides each sandbox's runtime-config document, so a renewal is a new
	// file and nothing more. Placed only on a driver whose URLs answer one.
	SandboxEdgeCredentialFile = "/etc/discobox/pool-agent/sandbox-edge-credential.json" //nolint:gosec // G101: a path, not a credential
)

// HandOverEnv, set to "1" in the environment a pool's maker runs `apply`
// and `units apply` with, has the box hand over to a new agent and proxy
// beside the running ones when what they run moves, rather than restarting
// them, so what the old ones serve is not cut (ADR 26-11-26-536 §§1, 4): a
// disco-vm pool machine's. It is not in the bootstrap, which an agent from
// before it would refuse to start on (pool-agent/REVIEW.md, "Bootstrap
// fields reach older agents"), and a bundle from before it ignores it.
const HandOverEnv = "DISCOBOX_POOL_HAND_OVER"

// UnitsStep is the pool-agent feature's step script that runs `units apply`,
// beside the agent's binary in the feature's directory: a pool's maker runs
// it after each apply where the bundle has it.
const UnitsStep = "units.sh"

// AgentBinary is where the pool-agent feature links the agent's binary,
// which the agent's, the proxy's and the mediator's units all run.
const AgentBinary = "/usr/local/bin/discobox-pool-agent"

// The pool agent and its proxy each run as an instance of a template unit,
// in one of two slots (ADR 26-11-26-536 §4): a handover starts the idle slot
// beside the running one, and the old one drains. Both slots serve the same
// sockets, which systemd holds (the units poolunits renders), so a restart or
// a handover refuses no connection.
const (
	AgentTemplate = "discobox-pool-agent@.service"
	ProxyTemplate = "discobox-proxy@.service"
	// UnitsService is what a pool's socket starts when traffic finds no slot
	// serving it: `discobox-pool-agent units start`, which starts the
	// enabled slot.
	UnitsService = "discobox-pool-units.service"
	// MediatorUnit is the builder's mediator, which its own sockets start.
	MediatorUnit = "discobox-buildkit-mediator.service"
)

// Slots are the two instances the agent and its proxy alternate between.
var Slots = []string{"a", "b"}

// AgentUnit is the pool agent's unit in slot. It is Type=notify: active
// once the agent has written what its proxy and builder read and said it is
// ready, which is before it serves.
func AgentUnit(slot string) string { return "discobox-pool-agent@" + slot + ".service" }

// ProxyUnit is the pool proxy's unit in slot, Type=notify as the agent's.
func ProxyUnit(slot string) string { return "discobox-proxy@" + slot + ".service" }

// AgentUnits are the agent's units in every slot: one of them is active
// once the pool's agent is up (`systemctl is-active` holds if any is).
func AgentUnits() []string {
	units := make([]string, 0, len(Slots))
	for _, slot := range Slots {
		units = append(units, AgentUnit(slot))
	}
	return units
}
