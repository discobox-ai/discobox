package proxyagent

import (
	"fmt"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/pprofserve"
)

// A pool's profiling listeners (ADR 26-11-24-418 §§1–2). On a pool with
// profiling on, the agent, the proxy and the BuildKit mediator each serve Go's
// pprof endpoints on a fixed loopback address of their own, behind a token
// written to layout.Root.PoolPprofToken. Fixed, not settings: the pool's loopback is its
// own, in its container's network namespace or its own machine, so nothing
// else competes for them, and a fixed address is what lets the agent's relay
// find a process without being told. They are beside ControlListenAddress for
// its reason: a sandbox never reaches the pool's loopback, and the proxy
// refuses it as a destination.
const (
	PprofAgentAddress    = "127.0.0.1:17091"
	PprofProxyAddress    = "127.0.0.1:17092"
	PprofMediatorAddress = "127.0.0.1:17093"
)

// The processes a pool profiles, as the relay's route names them.
const (
	PprofAgent    = "agent"
	PprofProxy    = "proxy"
	PprofMediator = "mediator"
)

// EnvProxyPprofListen and EnvMediatorPprofListen carry the proxy's and the
// mediator's profiling address in UnitEnvironmentFile, which both units read.
// The agent writes them on a pool with profiling on, and nothing otherwise.
const (
	EnvProxyPprofListen    = "DISCOBOX_PROXY_PPROF_LISTEN"
	EnvMediatorPprofListen = "DISCOBOX_MEDIATOR_PPROF_LISTEN"
)

// PprofAddress is the loopback address process serves its profiles on, if
// it is one a pool profiles.
func PprofAddress(process string) (string, bool) {
	switch process {
	case PprofAgent:
		return PprofAgentAddress, true
	case PprofProxy:
		return PprofProxyAddress, true
	case PprofMediator:
		return PprofMediatorAddress, true
	default:
		return "", false
	}
}

// PprofConfig is process's profiling listener on addr, named in its errors by
// setting. An empty addr serves nothing.
func PprofConfig(root layout.Root, projectID, poolID, process, setting, addr string) pprofserve.Config {
	return pprofserve.Config{
		Setting:   setting,
		Addr:      addr,
		TokenFile: root.PoolPprofToken(projectID, poolID, process),
		Remedy:    "something else on the pool holds the port; turn profiling off for the pool, or stop what holds it",
	}
}

// UnitPprofConfig is the proxy's or the mediator's profiling listener, from
// the address the agent wrote into its unit's environment under env. Unset
// serves nothing; an address that is not loopback is refused.
func UnitPprofConfig(root layout.Root, projectID, poolID, process, env string) (pprofserve.Config, error) {
	addr := strings.TrimSpace(os.Getenv(env))
	if strings.EqualFold(addr, pprofserve.Off) {
		addr = ""
	}
	if err := pprofserve.Validate(env, addr); err != nil {
		return pprofserve.Config{}, fmt.Errorf("profiling: %w", err)
	}
	return PprofConfig(root, projectID, poolID, process, env, addr), nil
}
