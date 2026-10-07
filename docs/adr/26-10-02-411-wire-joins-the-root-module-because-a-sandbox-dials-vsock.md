# 26-10-02-411 — `wire` joins the root module, because a sandbox dials VSOCK

- **Status**: Proposed (would supersede [0052](0052-iroh-is-an-optional-endpoint-scheme.md)
  §2's placement of `wire` and `vsock` in the pool-agent module)
- **Date**: 2026-10-02
- **Relates to**: [ADR 0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md)
  §4, whose bridge dial this placement serves.

## Context

ADR 0052 §2 split transport resolution into two packages by hop: `endpoint` in
the root module for client to control plane, and `pool-agent/wire` for the pool
agent's hop. It rejected folding `wire` into the root module because `wire`
reaches VSOCK through `pool-agent/vsock`, and moving it up "would put a Linux
guest transport and its third-party dependency into the module that the CLI,
hooks, and sandbox-agent all import". Its reasoning was that `wire` is the
guest-side hop and VSOCK's dependency belongs there.

ADR 0144 §4 adds a third hop: a sandbox in a host-VM pool reaches its pool's
proxy, credentials broker and the rest through a guest socket, not a network
interface. The in-sandbox `proxy/bridge` must therefore dial
whatever the URL in `bridge.json` names, `vsock` included, and it chooses that
transport through `wire` like everything else. `proxy/bridge` is a root-module
package: the sandbox-agent embeds it, and so does the pool agent's build
forwarder. Go would let it import `pool-agent/wire` (two modules may require
each other, and the package graph would have no cycle). The cost is the
module graph: the root module would require the pool-agent module, which
already requires the root. Pool-agent's requirements (moby, BuildKit and the
rest) would then join the root's module graph and take part in version
selection for every module that requires the root, the CLI included. That is a
far larger version of the cost 0052 §2 was avoiding.

The sandbox-agent is now one of `wire`'s own callers. It is a VM guest that
dials VSOCK, which is exactly the side 0052 said the dependency belongs to.

## Decision

`wire` and `vsock` move from the pool-agent module to the root module as
`wire` and `vsock`. The root `go.mod` takes `github.com/mdlayher/vsock` and
`github.com/mdlayher/socket`. `endpoint` and `wire` stay separate packages, by
0052 §2's other reasoning, which still holds: each hop's listen and dial
contracts answer to their own callers. `endpoint` carries iroh and DNS
resolution, and `wire` carries VSOCK.

Importing the root module does not link VSOCK. A binary carries the dependency
only if it imports `wire` or `vsock`: the pool agent, the server's VM providers
and the sandbox-agent do, and the CLI and hooks do not. The move costs the root
`go.mod` its two requirement lines. A module that requires the root without
importing `wire` or `vsock` records nothing for them, which is why the CLI's
`go.mod` is unchanged.

## Alternatives rejected

- **Leave `wire` in pool-agent and import it from there.** The dialer is in
  `proxy/bridge`, a root package. Importing `pool-agent/wire` from it makes the
  root and pool-agent modules require each other, and puts pool-agent's
  requirements into the version selection of every module that requires the
  root, as above. Moving the bridge into the sandbox-agent instead would leave
  the pool agent's build forwarder, which uses the same bridge, importing the
  sandbox-agent module.
- **Make `wire` and `vsock` their own nested module.** This keeps the
  requirement out of the root `go.mod`, at the price of another module, another
  `go.work` entry, and a root package depending on a nested module. Per the
  previous section, the requirement line is the whole cost being avoided, and a
  module boundary is out of proportion to it.
- **Inject a dial function into the bridge and keep the scheme vocabulary in
  pool-agent.** The sandbox-agent would then need its own scheme parser to build
  that function. That is the duplicate vocabulary 0052 §2 consolidated, and it
  contradicts 0144 §4's "chosen where every other transport is chosen".

## Consequences

- A new transport scheme is still taught to `wire` alone, and every hop that
  dials through it gets it, the sandbox's included.
- `vsock` drops `EnvControlPlanePort`, a pool-only constant nothing read, rather
  than carrying it into the root module.
- The VM image's guest-tools build copies `vsock` from the repository root.
