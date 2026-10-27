package sandboxruntime

import (
	"context"
	"crypto/ed25519"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
	"github.com/discobox-ai/discobox/pool-agent/gitcache"
)

// sandboxCore is the half of a runtime that is the same whatever a sandbox
// runs as: the pool's records of each sandbox in its tree (its runtime-config
// document, bundles, project layer, file secrets, harness fence and archive
// marker), the delivery of that document to the sandbox agent, the proxy
// policy and the credentials broker's environment it stages for the sandbox
// (publishCredentialEnvironment), the per-sandbox power lock and boot mark,
// the state and progress sinks, the sources' settling, and the pool's Git
// cache. A container (DockerSandboxRuntime) and a machine
// (MachineSandboxRuntime) each embed one, and give it what only they know
// through boxes.
type sandboxCore struct {
	// boxes is the runtime this core is the shared half of.
	boxes boxes
	// paths judges every path this runtime names inside a sandbox: by the
	// platform its sandboxes run on, the one the pool hosts (ADR 0145 §6).
	paths                 sandboxpath.Paths
	projectID             string
	poolID                string
	controlPlanePublicKey string
	// root is where this pool's state is, as this process sees it.
	root layout.Root
	// gitCache is this pool's cache of Git: one repository per remote, a
	// namespace per sandbox source (ADR 26-10-10-545 §2).
	gitCache *gitcache.Store
	// powerLocks serializes power operations per sandbox (see power.go).
	powerLocks sync.Map
	// powerHandover holds, per sandbox, whether its status last advertised
	// that the control plane may hold its power (ADR 26-10-16-337 §8). A
	// sandbox the poll has not reached since this agent started has no entry,
	// and keeps what its recorded document says.
	powerHandover sync.Map
	// booting holds the boot under way of each sandbox whose box is started
	// and whose sandbox agent has not answered yet (see beginBoot).
	booting sync.Map
	// statePublisher is the state channel's sink while a watcher is running
	// (see statereport.go). Power operations use it to announce a transition
	// they are about to make.
	statePublisher atomic.Value
	// progressPublisher is the same channel's sink for provisioning progress
	// that has no state transition to hang off — an image pull, above all
	// (see statereport.go, ADR 0039).
	progressPublisher atomic.Value
	// sandboxIdleTimeout is the pool policy's idle timeout, delivered in each
	// sandbox's runtime-config document; zero leaves the sandbox-agent's
	// default (ADR 0108).
	sandboxIdleTimeout time.Duration
	// poolEndpoints is where the pool serves its sandboxes, named in each
	// one's bootstrap: directly, or through the edge it sits behind.
	poolEndpoints sandboxconfig.PoolEndpoints
	// identityKey is the pool's identity key. It signs runtime-config
	// deliveries, and its public half is in every sandbox's bootstrap
	// (ADR 26-10-08-127 §3).
	identityKey ed25519.PrivateKey
	// runtimeConfigLocks serializes deciding and delivering each sandbox's
	// runtime-config document (see runtimeconfig.go).
	runtimeConfigLocks sync.Map
	// agentLayers is where the agent layer a delivered sandbox is placed
	// with comes from, and agents its extractions (see agentlayer.go).
	agentLayers AgentLayers
	agents      agentExtract
}

// boxes is what sandboxCore asks of the runtime it is the shared half of:
// what a sandbox's box is and how it is reached, which only that runtime
// knows.
type boxes interface {
	ListSandboxes(ctx context.Context) ([]*Sandbox, error)
	GetSandbox(ctx context.Context, sandboxID string) (*Sandbox, error)
	SandboxDialer(ctx context.Context, sandboxID string, port int) (Dialer, error)
	// takesRuntimeConfig reports whether the sandbox's box was made from a
	// bootstrap that names this pool's key, so its agent takes the pool's
	// runtime-config documents.
	takesRuntimeConfig(ctx context.Context, sandboxID string) (bool, error)
	// waitForSandboxAgent blocks until the agent of a box just started
	// answers, or the box reaches a state it will not answer from.
	waitForSandboxAgent(ctx context.Context, sandboxID string) error
	// stopRefusedSandbox stops a box whose agent refused the pool's
	// runtime-config delivery outright, from inside its boot.
	stopRefusedSandbox(ctx context.Context, sandboxID string, refusal error)
	// builtProjectLayer is the digest of the project layer the sandbox's box
	// was made with (projectLayerDigest).
	builtProjectLayer(ctx context.Context, sb *Sandbox) (string, error)
	// rebuildForProjectLayer makes the sandbox's box again on the project
	// layer now recorded for it, with no create request in hand
	// (settleConverged). The caller holds the sandbox's power lock.
	rebuildForProjectLayer(ctx context.Context, sb *Sandbox) error
}
