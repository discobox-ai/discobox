# 0126 — Remote sandboxes connect out to their pool

- **Status**: Proposed
- **Date**: 2026-09-17
- **Relates to**: [0006](0006-pool-is-the-runtime-host.md),
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  [0044](0044-builds-run-on-a-pool-shared-buildkit.md),
  [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md),
  [0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md), and
  [0123](0123-a-discobox-is-exported-as-its-spec-and-its-durable-tree.md).
  On acceptance, this decision narrows 0006's same-host rule and 0007/0094's
  pool-shared filesystem rules for remote-sandbox providers. The existing
  pool-hosted Docker providers keep those rules.

## Context

Every current backend eventually runs a pool-agent container and its sandbox
containers on one Docker daemon. The pool agent can bind its host directories
into those containers, address sandbox-agent by container IP, and use Docker
events and inspection to observe them. This is the implementation behind the
pool-local API; callers above that API do not speak Docker.

We want providers such as exe.dev and Archil to run one provider-managed
environment per Discobox sandbox. A separate provider-managed environment
hosts each Discobox pool's pool agent, proxy, BuildKit builder, registry, Git
origins, and other pool state. These environments need not share a filesystem
or a private Docker network. The existing sandbox-agent and harness OCI images
remain the runtime inside compute environments. This decision assumes they can
run the image's PID 1 initialization, systemd, and root-owned mounts inside
each environment. It does not require a shared mount between environments.

The pool is still useful: its proxy is the credential and egress boundary, its
BuildKit daemon is the shared build cache, and its Git origins receive client
pushes. A pool of remote sandboxes therefore spans several provider-managed
environments but retains one pool agent and one pool identity. This is the
specific exception to ADR 0006's assertion that the pool host is also the
host of every sandbox assigned to it.

Two current assumptions prevent simply substituting provider create/delete
calls for Docker calls:

- Pool-agent originates HTTP requests to sandbox-agent for readiness checks,
  standing status polls, exec/attach requests, and services. Its arbitrary
  HTTP port route instead dials the requested container port directly. Both
  paths must work without a pool-to-sandbox network connection.
- Pool-agent serves Git from local paths. Push-delivered bare origins are
  pool-side already, but the worktree and its Git repository are currently
  also on the pool host through mounts. `discobox apply` fetches from the
  worktree route, so making only the initial clone remote is insufficient.

Provider-published ports would let the pool dial sandbox-agent, but would
expose a separate endpoint for every compute sandbox and bind the control path
to provider-specific ingress and authentication behavior. We instead need one
way for sandbox-agent to be reachable through the pool regardless of whether
the provider offers private addressing or inbound ports.

## Decision

### 1. One pool environment, one compute environment per sandbox

The control plane creates and reconciles one provider environment for each
Discobox pool. It runs the existing pool-agent process and preserves its
`/var/lib/discobox` state on that environment's own durable filesystem.
There is no host bind of `/var/lib/discobox` required. Pool registration,
identity, heartbeat, proxy, BuildKit, registry, and the pool-local operations
API remain pool-agent responsibilities.

The pool agent creates and operates a separate provider environment for each
Discobox sandbox through the selected provider's API. The environment boots
the pinned harness image, including the existing sandbox-agent. Pool-agent
remains the authority for which provider sandbox belongs to its pool, and
reports observed power and provisioning state to the control plane as it does
today. A provider resource is identified by the Discobox sandbox ID and pool
ID in provider metadata where supported; the pool agent also durably records
the provider's opaque resource ID. List/reconcile must recover a resource
created before a response or local state write was lost. It must never create a
second billable sandbox merely because the first create response was lost.

This is one remote runtime design, with a required provider backend interface
for create, inspect/list, start, stop, delete, transfer, and connection
operations. exe.dev and Archil implement that interface separately. Common
source, config, proxy, readiness, Git, archive, and pool-local API behavior
belongs above the backend. Do not add optional interfaces to disguise a
missing operation: a backend unable to meet a required lifecycle or transfer
contract cannot offer this provider mode.

### 2. Sandbox-agent establishes the connection to pool-agent

Once systemd starts sandbox-agent, it opens a persistent outbound connection
to a pool-agent listener. The pool agent authenticates the connection with
the sandbox's pool-issued client certificate and binds it to the pool and
sandbox IDs. The provider's public endpoint, if one is used to reach the pool,
is only transport; its authentication is not the Discobox authorization
decision. The pool listener must reject a certificate for another pool or
sandbox, an unknown or archived sandbox, and a connection after that
sandbox's credential has been revoked.

The connection carries multiplexed streams: pool-agent can originate HTTP
requests to sandbox-agent over it, including upgrades and long-lived attach
streams, while sandbox-agent initiated traffic is not granted the authority
to issue pool-local administrative requests. A WebSocket carrying the same
kind of multiplexed byte streams already used by `pool-agent/cpmux` is a
suitable transport; the wire contract must preserve request cancellation,
half-close, backpressure, and WebSocket streaming. The outbound dial is a
transport reversal, not a reversal of who may call sandbox-agent's API.

At most one connection is authoritative for a sandbox at a time. A reconnect
replaces and closes the old session, and in-flight requests on the old session
fail rather than being silently replayed on the new one. Pool-agent waits for
the authenticated session and a successful sandbox-agent health response
before reporting create/start ready. A lost session makes agent-dependent
operations temporarily unavailable and triggers bounded reconnect/backoff;
it does not by itself prove that the provider sandbox stopped. Provider
inspection remains the authority for power state, and pool-agent's existing
status poll resumes through the new session without requiring sandbox-agent
to push status to the control plane.

Server and CLI requests continue to enter through the existing pool-local
routes and authorization scopes. The pool agent routes them across the
connection to sandbox-agent. Status polling uses only its server-minted
`status:read` token, preserving ADR 0030's narrow background authority.
Sandbox-agent still validates its own tokens on every forwarded API request;
the transport certificate alone does not make an exec or attach request
authorized.

The pool-local `/http/{port}` route is a separate case: today it proxies
directly to an arbitrary port, not to sandbox-agent's API. For a remote
sandbox, after checking the existing `sandbox:http` scope and port syntax,
pool-agent opens a stream on the authenticated session and asks sandbox-agent
to dial that port on the sandbox's own network-facing interface address. The
remote backend supplies or discovers that address inside the sandbox; using
`127.0.0.1` alone would break a service bound only to its interface address,
which today's container-IP route can reach. The stream carries
the HTTP connection bytes in both directions, including upgrades, with no
pool-side interpretation of the service protocol. This is an internal
pool-agent request, not a new user-accessible way to claim `tcp:connect`;
the pool retains the existing route's user authorization before opening it.
The existing `tcp/attach` and `udp/attach` API routes remain sandbox-agent
operations forwarded over the same session with their own scopes.

### 3. Provider file transfer replaces cross-environment mounts

Each compute environment has private `/.discobox/data`, `cache`, `config`,
`sources`, and `secrets` roots. Sandbox-agent's PID 1 flow still resolves the
image's declared volumes and binds paths **within that same environment**.
The provider creates or retains these roots in the sandbox filesystem rather
than mounting pool-host directories into it. `data` and `cache` persist only
for that sandbox according to the provider's disk lifecycle. A declared
`scope: shared` cache path, including `/nix`, is shared by processes in that
one sandbox, not between the pool's sandboxes. The pool-wide BuildKit solver
cache remains shared because it lives in the pool environment, outside these
roots. Source-scoped pool data currently mounted at
`/.discobox/data-per-source/<slug>` is unavailable in this mode; it must not
be represented by an empty directory with the same claimed sharing semantics.

Pool-agent renders the effective `sandbox.json` and pool-specific proxy
material, then transfers them to the sandbox's config root. The basic config
must be present before PID 1 wires volumes and execs systemd. If a provider
offers file transfer only after starting the environment, its bootstrap must
keep PID 1 waiting for the complete config and then exec the normal
`discobox-sandbox-agent init`; starting systemd first and attempting the PID 1
work later is not equivalent. Pool-agent uploads files through temporary names
and publishes an explicit completion signal only after the whole set is
durable. A partial transfer never clears a readiness gate.

The existing secret model stays: secret-bound environment values in the
sandbox are sentinels, not provider credentials or resolved customer secret
values. Pool-agent keeps the sentinel registry and resolution policy, and
copies the root-owned `secrets.json` and proxy client material into the
sandbox. An initial secrets-ready gate holds the first harness launch and
repository services until those files and the proxy bridge are usable. It
does not block sandbox-agent itself: the pool agent needs the agent connection
to observe readiness and serve other operations. Later secret assignment,
rotation, and grant changes update the file atomically and use the existing
watch path without restarting the sandbox. Copy APIs must preserve modes or
pool-agent must set and verify them through root command execution. The
provider API credential stays in the pool environment, never in a compute
sandbox or its public config.

### 4. Git origins stay in the pool; compute sandboxes clone over HTTP

For a local client source, the existing server → pool-agent Git push places
the pinned commit and optional dirty-workspace snapshot ref in a pool-side
bare origin. It does not require a compute sandbox to exist first. This
removes the current create-before-push cycle **for remote runtimes**: their
pool-local create first records the sandbox and provisions its bare origins,
then returns without waiting for source delivery or starting compute. The
remote `git-origins` route reads that pool-side record and does not invoke
compute auto-start. It still checks the caller's pool token and the requested
repository belongs to that sandbox. The Docker runtime keeps its existing
create-before-push and `autoStart` behavior. A remote URL source may still
clone its actual remote directly; no local source directory is reachable by
mount, so `LocalSourceRoots` is empty for these provider instances.

After delivery, the compute sandbox fetches or clones its pool-side origin
over authenticated Git HTTP into its private `sources/<slug>` tree. Its
`origin` remote is that pool URL, not `/.discobox/origins/<slug>`. Future
`discobox push` updates the same bare origin, so `git fetch origin` inside the
sandbox retains its meaning. The pool Git listener uses the sandbox's mTLS
identity to allow read access only to that sandbox's sources. A sandbox's
outbound localhost bridge can hold the client key, but sending Git to the
ordinary HTTP egress proxy does not by itself authenticate a Git route:
pool-agent must terminate and authorize this internal Git request explicitly.
Client pushes keep their existing user-scoped pool API authorization.

Pool-agent verifies the pinned commit and every required source before
clearing the source-ready gate. It resolves the project's source-contributed
config before declaring the final sandbox spec settled, including any
dirty-workspace snapshot that changes `.discobox/project.json`. It may
materialize a pool-local staging copy for that calculation; that staging tree
is not mounted into the compute sandbox. Clone completion alone is not the
ready signal, because a project layer can require a different final config
or rebuild. The first harness launch and repository services wait until both
source and secret readiness are satisfied.

The worktree Git route cannot keep using `git http-backend` on a pool-local
path. To preserve `discobox apply` and other worktree fetches, pool-agent
forwards that route to an authenticated Git endpoint in sandbox-agent over
the outbound session. The endpoint serves the sandbox's own worktree
repository with the existing read/write scope distinctions. It cannot let a
sandbox name another sandbox's repository. The pool-side origin route remains
local to the pool agent. This is a deliberate split between two repositories
that happen to have similar HTTP transports.

### 5. Pool services are reached through authenticated network paths

The pool environment still runs the egress proxy, BuildKit daemon and
mediator, output registry, and credential service. Compute sandboxes reach
them over network connections through sandbox-local bridges holding their
pool-issued client certificate. The bridges keep keys out of ordinary
non-root processes and preserve the existing sandbox-ID identity at the
pool boundary. TLS authenticates the pool service as well as the sandbox;
provider-assigned hostnames or public URLs must not replace certificate name
verification. Credentials are renewed and transferred before their old
certificates expire, without restarting the sandbox.

The `docker build` shim still selects the pool BuildKit mediator; build
context and secrets travel over the BuildKit session, and build output is
pulled from the pool registry into the sandbox's own Docker daemon. `docker
run` remains local to that daemon so its bind paths refer to the sandbox's
filesystem. The pool's registry currently relies in part on a private Docker
network and unguessable repository names. Across provider environments it
must authenticate and authorize requests, including pulls and deletion, and
must not treat an unguessable path as sufficient protection on a provider
published endpoint. Pool build-step egress keeps its per-sandbox attribution
through the existing mediator and proxy identities.

The pool environment needs an address reachable from its compute sandboxes
for these services and for the outbound agent connection. It may use a
provider HTTPS endpoint or a separate relay, but all externally reachable
listeners authenticate before serving pool data. Providers that impose an
egress policy must permit the pool address and the intended proxied egress.
The control plane's own connection to pool-agent remains provider-managed as
today; no compute sandbox receives a control-plane administrative credential.

### 6. Lifecycle and export keep their existing meanings

Stop/start act on the provider compute environment and preserve its private
disk. Archive removes or stops the live compute runtime while retaining the
durable per-sandbox tree; unarchive reinstantiates the pinned image and
adopts that tree. Delete confirms removal of both provider runtime and its
durable data before the control plane removes the row. A provider's automatic
stop, pre-emption, or TTL expiry is observed power state, not permission to
discard the sandbox. Pool-agent retries idempotently and reports a failed
recovery rather than replacing an inaccessible disk with an empty one.

The export format from ADR 0123 remains the portable contract: spec plus
`data`, `sources`, and push-delivered `origins`, with verified checksums.
Pool-agent supplies `origins` from its own durable tree and obtains `data`
and `sources` from the compute sandbox's durable disk. An export still refuses
a running sandbox. The outbound agent session therefore cannot be the only
way to read its tree: the provider backend must read a stopped disk, provide
a consistent snapshot, or support a quiesced transfer that preserves the
existing stopped-export guarantee. Import restores both halves before the
new sandbox is allowed to start. A backend with no such path does not satisfy
the remote runtime contract.

Pool cache clearing stops the pool's compute sandboxes, clears the pool
proxy/BuildKit/registry caches, and clears each sandbox's private cache
through provider file operations or an equivalent stopped-disk operation.
It reports which sandboxes it stopped. Resource reports distinguish the pool
environment's own CPU, memory and disk from provider-reported compute
environments; the pool's host counters can no longer be interpreted as the
sum of all its sandboxes.

### Provider applicability

exe.dev and Archil are the first intended backends, not reasons to put either
provider's API shape in the common runtime contract. exe.dev documents
creation from a custom OCI image, a persistent VM filesystem, and SSH-based
commands and file transfer
([image creation](https://exe.dev/docs/customization),
[API](https://exe.dev/docs/api),
[filesystem](https://exe.dev/docs/serverful)). The exe.dev backend must still
establish a stable pool endpoint and prove the bootstrap, stopped-tree
transfer, and identity rules above using those mechanisms.

Archil documents persistent sandboxes with public OCI base images, private
durable disks, process execution, and HTTPS service endpoints. Its published
constraints include ARM64 images, time-to-live shutdown, possible preview
pre-emption, plan-dependent outbound networking, and publicly reachable
services that require application authentication
([persistent sandboxes](https://docs.archil.com/compute/sandboxes/introduction)).
Those are backend facts to validate against this contract: the Discobox
images must be available for its architecture and registry policy; TTL and
pre-emption must preserve the disk; and the pool endpoint must authenticate
every caller. Neither backend may claim support based solely on create and
exec APIs. The required stopped-tree transfer and bidirectional streaming
semantics must be demonstrated for each backend before it is enabled.

## Alternatives rejected

**Run the pool agent and every sandbox on one provider environment, using
nested Docker.** This retains mounts and the current Docker runtime, but
returns to one pool host as the isolation boundary and makes the provider's
per-sandbox lifecycle, disk, and sizing APIs irrelevant. It is a different
product topology from the one being added.

**Replace pool-agent with a direct control-plane provider.** This avoids the
pool environment but also displaces the pool proxy, shared BuildKit cache,
registry, Git origins, pool-local API, and status reporting. Rebuilding those
in the server or each sandbox is a larger ownership change than adapting the
runtime behind pool-agent.

**Expose each sandbox-agent as a provider HTTPS service and have pool-agent
dial it.** Provider services differ in ingress controls and address lifetime;
some endpoints are public. It would introduce one externally reachable
control endpoint per sandbox and make the central attach/status path depend on
provider ingress. The outbound connection needs only a reachable pool
endpoint and keeps sandbox-agent behind the pool's authorization boundary.

**Have sandbox-agent push status and receive commands through unrelated
provider APIs.** Separate reporting and command transports would split
readiness, attach, status, and failure handling across two protocols. The
multiplexed connection preserves the existing pool-agent → sandbox-agent
request semantics. Merely establishing it is not authority for sandbox-agent
to originate control-plane requests, preserving ADR 0030's trust decision.

**Copy source files directly into every compute sandbox.** A file copy alone
loses the pool-side Git origin that `discobox push` updates and the Git refs
that represent dirty-workspace snapshots. Retaining the origin and cloning
over Git HTTP gives the sandbox normal fetch/rebase behavior without shared
storage.

**Share a provider disk between pool and compute sandboxes.** It would
recreate the host mount dependency and make correctness depend on provider
filesystem consistency, ownership, and isolation rules. The network
protocols above have explicit identities and work with private disks.

**Point each sandbox's Docker CLI at the pool's Docker daemon.** Build inputs
can travel to pool BuildKit, but `docker run` bind paths and daemon state
belong to the compute sandbox. Moving all Docker commands to the pool would
run the user's containers in the wrong environment and weaken isolation.

## Consequences

- The existing pool-local API and control-plane resource model can remain,
  while `pool-agent/sandboxruntime` gains a remote implementation and
  sandbox-agent gains the outbound transport and worktree Git endpoint.
- Network loss is a distinct state from provider power loss. Requests through
  a disconnected agent fail or wait within their existing readiness budgets;
  pool-agent must not infer deletion from a broken session.
- A pool repair must preserve its identity key, Git origins, BuildKit state,
  registry, and proxy trust material. If that durable pool tree is lost,
  compute disks alone do not reconstruct the pool's Git and credential state.
- Private per-sandbox cache and loss of source-scoped shared data change the
  behavior of remote pools. They must be described as such in provider
  configuration and user documentation when this mode ships.
- The pool environment holds provider management credentials capable of
  operating its compute sandboxes. They must be root-only and scoped to the
  pool's resources where the provider allows it. A backend with only an
  account-wide key has a larger pool-host compromise radius that must be
  made explicit before it is enabled.

## Deferred

- **Cross-sandbox filesystem cache and source data.** Revisit only if a
  provider-neutral, isolated sharing mechanism is required by real workloads.
  This decision deliberately retains shared BuildKit and proxy caches while
  each compute sandbox's filesystem cache is private.
- **Provider-native snapshots and forks as user features.** They may optimize
  restore or export, but do not change the portable durable-tree contract or
  become a substitute for confirmed delete and transfer.
