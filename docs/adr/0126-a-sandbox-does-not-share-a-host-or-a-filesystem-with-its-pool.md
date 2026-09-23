# 0126 — A sandbox does not share a host or a filesystem with its pool

- **Status**: Accepted
- **Date**: 2026-09-23 (rewrite of the 2026-09-17 draft, which was never
  accepted and against which nothing was implemented)
- **Relates to**: [0006](0006-pool-is-the-runtime-host.md),
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0026](0026-local-source-origin-is-bind-mounted-live-into-the-sandbox.md),
  [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  [0044](0044-builds-run-on-a-pool-shared-buildkit.md),
  [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md),
  [0093](0093-a-local-sources-origin-is-its-git-directory.md),
  [0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md),
  [0123](0123-a-discobox-is-exported-as-its-spec-and-its-durable-tree.md), and
  [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md).
  On acceptance it narrows 0006's same-host rule and 0007/0094's pool-shared
  filesystem rules for any sandbox that is not a container beside its pool. It
  supersedes 0026 §§1-2: a local source's origin is still the developer's live
  Git directory, as 0093 §1 has it, but it is served over Git HTTP rather than
  bind-mounted into the sandbox.

## Context

Every backend today ends with a pool-agent container and its sandbox
containers on one Docker daemon. The pool agent binds its own host directories
into each sandbox, addresses sandbox-agent by container IP, observes power
state from Docker events, and confines egress with an `Internal` network.

Those bind mounts do two unrelated jobs, and only one of them is about
storage.

**They are how the two agents talk.** The pool agent renders `sandbox.json`
at create and again before every start to carry the idle timeout, rewrites
`secrets.json` whenever a secret is bound or rotated, publishes the
source-readiness marker as a file, and stages proxy client material and the
registry namespace the same way. In the other direction it runs `git` inside
the sandbox's own checkout on every create, reads the delivered source's
`.discobox/project.json` to settle the sandbox's final spec, and serves that
worktree to clients with `git http-backend` over a pool-local path. A local
source's origin is a third mount: the developer's own Git directory, bound
read-only into the sandbox at `/.discobox/origins/<slug>` (ADR 0026, 0093).

**They also decide where durable storage lives**, which is what lets the pool
agent archive, export, restore, reap and measure a sandbox's tree.

Two kinds of sandbox break the first job. A provider-hosted sandbox —
exe.dev, Archil — shares no host with its pool at all. A host-VM sandbox
([0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md)) could
share a filesystem through the hypervisor's file sharing, and deliberately does
not: it would put the host's filesystem inside the guest's reach and make
correctness depend on that layer's ownership and consistency rules, for a
channel that has to exist over the network anyway.

Keeping the mount as the channel is therefore not a backend detail. Every fact
the pool needs to tell a sandbox would be a file write that works on one
backend and silently does nothing on the others — and "silently" is the word:
an absent file and an empty directory read exactly like a sandbox that declared
nothing.

## Decision

### 1. The mount is never the channel

Everything the pool tells a sandbox, and everything it reads back from one, is
a request on an authenticated connection between the two agents. A backend may
still use a mount, a disk or a copy as an implementation of *storage*; none of
them is an implementation of *communication*.

This holds for every backend, the Docker one included. A pool-to-sandbox fact
that travels as a staged file on Docker and as a request everywhere else is two
mechanisms for one job, and the file-shaped half is the one that fails without
an error.

The rule for new work follows from it: if the pool has to tell a sandbox
something, it is a route on the sandbox-agent API. If the pool has to know
something about the sandbox's own tree, it asks the sandbox.

There is one exception, and it is about timing rather than mechanism: a backend
may place files before the agent exists, because something has to. That is
bootstrap, and §3 bounds it. Once the agent is running, the pool changes
nothing inside a sandbox except by asking it.

### 2. Each sandbox owns its roots

A sandbox has its own `/.discobox/data`, `cache`, `config`, `sources` and
`secrets`, on its own storage. Where the runtime has a PID-1 flow, that flow
still resolves the image's declared volumes and wires them **within that same
environment** (ADR 0007); where it has none, the declared-volume mechanism does
not apply at all.

`cache` is private to the sandbox, and a `scope: shared` path — `/nix` among
them — is shared between that sandbox's own processes rather than between the
pool's sandboxes (ADR 0094 describes a pool-shared tree that no longer exists
here). The pool-wide BuildKit solver cache is unaffected: it lives in the pool,
not in these roots.

Source-scoped pool data (`/.discobox/data-per-source/<slug>`) is unavailable,
and must not be represented by an empty directory claiming the same sharing.

### 3. Config, secrets and readiness converge on one intake

The pool's whole view of a running sandbox travels as one document with a
revision, not as a route per fact:

```text
PUT .../sandboxes/{id}/runtime-config    what the pool wants this sandbox to be
GET .../sandboxes/{id}/runtime-config    what the agent has applied
```

It carries the config layers the pool owns (the idle timeout among them), the
secret environment map, the proxy client material and CA bundles, the registry
namespace, and each source's origin address, pin and delivery state. The agent
applies it by writing the same local files it reads today, so `config`,
`secretswatch` and `sourcesready` are untouched — what changes is who writes
them, not what watches them — and it reports the revision it applied, which
rides the status poll that already runs every fifteen seconds (ADR 0030).

**The document is whole rather than incremental**, because that is how the rest
of this system converges (ADR 0017). A delta that is lost is lost silently and
the sandbox drifts; a document that is lost is superseded by the next one. It
also makes the re-drives that already happen — resume, re-pin, repair — an
idempotent convergence instead of a second write, which is what the idle
timeout's rewrite-before-every-start exists to be today. And it removes the
ordering questions between separately delivered facts: there is one order,
the document's.

A delivery is atomic, and a partial one never clears a gate. The agent keeps
the last document it applied, so a restart does not wait on the pool to know
what it is.

**Bootstrap is the smallest thing a backend places before the agent exists**:
the sandbox's identity, where its pool is, and the trust material for that hop.
Everything after that is the intake. Where a runtime wires volumes before the
agent starts — a Linux container's PID-1 flow — the config must be complete
before that flow runs, so either the bootstrap carries it or the flow fetches
it before exec'ing the real init; starting the init first and doing the PID-1
work afterwards is not equivalent. On a Docker pool the config volume is still
that placement, which is why §1's exception exists.

The secrets model is unchanged: what reaches a sandbox is sentinels, never
resolved values and never a provider credential. A readiness gate holds the
first harness launch and the repository's declared services until the secrets
and the proxy hop are usable; it does not hold sandbox-agent itself, which the
pool needs up in order to observe anything at all.

### 4. The origin stays where it is; the sandbox's route to it changes

A local source's origin is still the developer's live Git directory, exactly as
ADR 0026 and 0093 have it, and the pool still reaches it the way it does today
— through its host mount prefix on a Docker pool, or by opening the path on a
host pool, where the agent runs on that machine (ADR 0144). What ends is the
bind into the sandbox.

The sandbox clones and fetches over authenticated Git HTTP served by the pool,
and its `origin` remote is that URL. The repository behind it is the
developer's own, so new commits are visible to a `git fetch origin` inside the
sandbox without anyone pushing, which is the property ADR 0026 exists for.

**Going through the protocol is what narrows the exposure.** A bind hands over
the whole Git directory: every branch, the reflogs, the index, objects no ref
reaches — the credential committed and rewritten out of history is still in
there — and `.git/config`, which in real setups carries `http.<url>.extraheader`
and tokenized remote URLs. 0093 narrowed the bind from the working tree to the
Git directory; this narrows it to what `git-upload-pack` serves, which is the
advertised refs and the objects reachable from them, and none of the above.

Two properties that the read-only bind gave by accident are now stated:

- **Fetch-only.** The sandbox's route to a live local origin serves
  `git-upload-pack` and never `git-receive-pack`. Work leaves a sandbox the way
  it does today, through `apply` and the worktree route.
- **The served refs are an allow-list, and `HEAD` is on it.** Everything under
  `refs/` is advertised by default — `refs/stash` and every other branch
  included — so a live origin serves `HEAD`, the branch `HEAD` names, and the
  refs the source itself declares (its branch and its base), and nothing else.
  `HEAD` is on the list because nothing else can put it there: no client push
  updates a live origin, so `HEAD` is how the sandbox learns where the
  developer is now and what `git rebase origin/<branch>` is rebasing onto. The
  source's own refs are on it so that a developer switching branches cannot
  take the sandbox's base out from under it. Hiding a ref means nothing unless
  the server also refuses to serve an object asked for by name, so fetching an
  unadvertised object id stays off; otherwise the allow-list is decoration.

**A pool that cannot see the developer's repository serves a bare origin
instead** — the pool-side repository the client pushes into (ADR 0058), which
is already how a linked worktree, a submodule checkout and a non-absolute path
are delivered. The sandbox cannot tell the difference: it is given an address
and clones from it. The fork lives in the pool, where it is a fact about what
the pool can reach, rather than in the sandbox, where it would be a second code
path through the most delicate step of a create.

**Materializing is convergence, not a verb.** The source list in §3's document
says where each source's origin is, what commit is pinned, where it belongs and
whether delivery has landed; the agent converges on that and reports per-source
state. The marker that makes materializing once-only already lives inside the
repository's own `.git`, so the agent reads its own marker rather than the pool
reading it across a boundary, and re-cloning over work the sandbox has done
stays forbidden. The clone runs as the user that owns the checkout, in the
sandbox's own namespace, which is what retires the pool-side chown threading
and `safe.directory` injection that exist only because the pool operates on
files it does not own. The pool still needs the sandbox's ids for the reasons
ADR 0141 gives; cloning and chowning a checkout stops being one of them.

A source that is a remote URL still clones that remote directly, with ADR 0128's
lent credential where it is private.

The worktree route cannot keep running `git http-backend` on a pool-local path.
It is forwarded to an endpoint in sandbox-agent that serves the sandbox's own
repository, with the same read and write scope distinctions, and a sandbox
cannot name another sandbox's repository. The origin route stays local to the
pool agent: two repositories with similar transports, deliberately split.

The pool still settles the sandbox's final spec before the source-ready gate
clears, which means reading the delivered source's project layer — a read it
asks the sandbox for, or performs on a pool-local staging copy that is never
mounted into the sandbox. A completed clone is not by itself the ready signal
(ADR 0055).

Serving a live repository races the developer's own `git gc`, so a fetch can
fail on an object that was just pruned. That is a property the bind has today,
not one this introduces.

### 5. Reachability is the backend's to supply

A backend must be able to hand the pool agent a connection to a sandbox's
agent. How is the backend's own business, and three shapes exist:

- **A guest socket**, where the agent runs on the sandbox's own machine
  (ADR 0144). The pool dials in; there is no network.
- **An address the pool can dial**, where the backend gives the sandbox private
  addressing the pool shares.
- **A session the sandbox opens outward**, for a backend that offers no inbound
  path at all. That session carries multiplexed streams, preserves
  cancellation, half-close, backpressure and upgrades, and at most one is
  authoritative for a sandbox at a time — a reconnect replaces the old one, and
  in-flight requests on it fail rather than being replayed.

The multiplexed session is the expensive shape, and it is required only by the
backends that cannot be dialed. Mandating it everywhere would make a pool that
can simply open a socket pay for a protocol it does not need.

Whatever the transport: sandbox-agent still validates its own tokens on every
request, and transport identity is never the authorization decision. Status
polling uses only its server-minted `status:read` token (ADR 0030). A lost
connection is not evidence that the sandbox stopped — power state comes from
the runtime, not from the channel.

The intake of §3 and the Git hops of §4 are ordinary requests on whichever of
these a backend supplies. Nothing above the transport knows which one it got.

### 6. The sandbox has no egress but the proxy

A sandbox's only route off its machine is the pool proxy. In a Docker pool the
`Internal` network is what makes that true; elsewhere the backend owes the same
property — no NIC, no route, or a policy that permits nothing else.

A backend that cannot provide it does not get the property by asking the
sandbox nicely: unsetting `HTTP_PROXY` would then walk past the audit trail and
the traffic policy. Sentinels stay safe, since a sentinel is worthless to
anything but the proxy, but the audit record is not a record any more. Such a
backend is not enabled until it can, and if it ever is, the pool must record
that its audit and policy are advisory there.

### 7. Pool services are reached over authenticated network paths

The proxy, the credential broker, and — where the pool has them — the BuildKit
mediator and output registry are reached over connections carrying the
sandbox's pool-issued client certificate, through sandbox-local bridges that
keep the key away from ordinary processes. TLS authenticates the pool service
as well as the sandbox; a provider-assigned hostname does not replace
certificate verification. Credentials are renewed before expiry without
restarting the sandbox.

The registry's unguessable repository names (ADR 0047) are not protection once
it is reachable off a private network: it authenticates pulls and deletions, or
it does not leave the pool. Per-build egress attribution is unchanged. A pool
whose sandboxes run no nested Docker has no builder and no registry at all
(ADR 0144 §3).

### 8. Lifecycle and export keep their meanings

Stop and start act on the sandbox's runtime and preserve its storage. Archive
drops the live runtime and keeps the durable tree; unarchive adopts it. Delete
confirms that runtime and data are both gone before the control plane drops the
row (ADR 0022). A backend's own automatic stop, TTL or pre-emption is observed
power state, never permission to discard a sandbox: the pool retries and
reports a failed recovery rather than replacing an unreachable disk with an
empty one.

The export contract is unchanged (ADR 0123, 0129): the spec plus `data`,
`sources` and the pool's `origins`, checksummed, refused while the sandbox
runs. The agent connection cannot be the only way to read a tree, precisely
because an export requires a stopped sandbox — so a backend must be able to
read a stopped disk, snapshot it consistently, or quiesce it. Import restores
both halves before the sandbox may start. Clearing the pool's caches stops the
sandboxes, clears the pool's own caches, and clears each sandbox's private
cache through the same channel or a stopped-disk operation.

## Alternatives rejected

- **Require file sharing from every backend.** A hypervisor can share a
  directory into a guest, so a host-VM pool could keep the mounts. It puts the
  host's filesystem inside the guest's reach, makes ownership and consistency
  the sharing layer's business rather than ours, and does nothing for
  provider-hosted sandboxes — so the request-shaped channel would have to exist
  anyway, and would exist twice.
- **Leave the Docker runtime on the mounts and add a second, transfer-based
  runtime beside it.** It is the smaller diff and the worse system: two ways
  for the pool to tell a sandbox something, one of which is exercised by every
  developer and the other only by the backends nobody runs locally. The failure
  mode is a feature that works on the machine it was written on and silently
  does nothing anywhere else.
- **One mandated transport for every backend.** The earlier draft of this ADR
  required the outbound multiplexed session everywhere. It buys uniformity and
  costs a session protocol — mux, cancellation, half-close, backpressure,
  reconnect authority — that a pool able to dial its sandbox directly does not
  need. §5 keeps the session for the backends that have no other option.
- **Expose each sandbox-agent as a provider HTTPS service.** One externally
  reachable control endpoint per sandbox, with the central attach and status
  path depending on provider ingress behavior and address lifetime.
- **Have sandbox-agent push status and take commands over unrelated provider
  APIs.** Readiness, attach, status and failure handling would split across two
  protocols, and sandbox-agent would originate control-plane requests — the
  trust decision ADR 0030 made deliberately.
- **Keep binding the developer's Git directory into the sandbox where the
  filesystem allows it.** It is free on a Docker pool and it is the shape with
  the widest exposure: read-only stops the sandbox corrupting the repository
  and not the sandbox reading `.git/config`, the reflogs, or every object the
  history no longer reaches. It is also the one delivery shape that cannot
  exist on a pool that is not on the developer's machine, so keeping it forks
  the sandbox's own clone path — and it buys nothing that §4 does not, since
  the pool serves the same live repository either way.
- **A route per fact instead of one document.** Setting the idle timeout,
  pushing secrets, flipping readiness and renewing certificates as separate
  imperatives makes the caller responsible for ordering them and makes every
  lost call a silent divergence. Convergence on a revisioned document is what
  the rest of the system already does.
- **Copy source files into the sandbox instead of keeping a Git origin.** It
  loses the repository `discobox push` updates and the refs that carry a dirty
  workspace, and with them ordinary `fetch`/`rebase` inside the sandbox.
- **Share one disk between the pool and its sandboxes.** It recreates the host
  mount dependency with the provider's filesystem semantics underneath it.
- **Point each sandbox's Docker CLI at the pool's daemon.** Build inputs can
  travel to a pool builder; `docker run` bind paths and daemon state belong to
  the sandbox.

## Consequences

- `pool-agent/sandboxruntime` gains backends beside the Docker one, and the
  Docker one loses its post-create file staging: what it writes into a config
  or secrets volume after the agent is up becomes the intake, and the volume
  keeps only the create-time placement of §3.
- sandbox-agent gains the intake, the source convergence and clone that were
  the pool's, a worktree Git endpoint, and — for backends with no inbound path
  — the outbound session. The pool loses the git it ran in someone else's
  checkout, and the ownership workarounds that came with it.
- Every sandbox reaches its origin over the same URL-shaped remote, so
  `discobox apply`, `git fetch origin` and a rebase behave the same whether the
  origin behind it is live or a pushed bare repository.
- Network loss and power loss become distinct states. Requests through a
  disconnected agent fail or wait within their existing budgets; nothing infers
  deletion from a broken connection.
- A private per-sandbox cache and the absence of source-scoped shared data
  change how such a pool behaves, and have to be said plainly in provider
  configuration and user documentation.
- A pool's durable tree — identity key, origins, proxy trust, and whatever
  build state it has — is not reconstructible from its sandboxes' disks. Repair
  must preserve it.
- A provider-hosted backend holds credentials able to operate its sandboxes.
  They stay in the pool, root-only, scoped to the pool's own resources where
  the provider allows it; a backend with only an account-wide key has a larger
  blast radius that is stated before it is enabled.
- exe.dev and Archil are the first provider-hosted candidates, and neither is
  enabled on the strength of a create-and-exec API: the stopped-tree read, the
  egress property of §6, and the streaming semantics of §5 are demonstrated per
  backend.

## Deferred

- **Cross-sandbox filesystem cache and source data.** Revisited only if a real
  workload needs it and a provider-neutral, isolated mechanism exists. Shared
  BuildKit and proxy caches stay; the filesystem cache does not.
- **Provider-native snapshots and forks as user features.** They may make
  restore or export cheaper without changing the portable tree contract, and
  never substitute for a confirmed delete or transfer.
