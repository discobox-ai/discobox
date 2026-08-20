# 0063. A pool agent keeps its identity key, and registers once

Status: Accepted

Date: 2026-08-19

## Context

A pool agent's Ed25519 keypair is the pool's identity. It is generated at
startup by `GenerateKeySource`, published to the control plane during
registration, recorded as `Pool.PublicKey`, and thereafter used to sign the
PASETO assertion that authenticates *every* agent-to-control-plane request
(`poolauth.CreateToken` / `auth.VerifyToken`).

The key exists only in the agent's memory, so it is regenerated on every agent
process start, and `bootstrap.Run` therefore registers unconditionally on every
start. Registration spends a bootstrap token, which is deliberately single-use
and short-lived: `Store.RegisterPool` rejects a token that is used, revoked, or
expired, and a sweep deletes tokens that are any of those.

Nothing re-mints. `Engine.ensurePoolContainer` returns without calling `mint`
when the existing container is healthy — a deliberate choice, so a steady-state
drift check does not persist a single-use credential — and the bootstrap is not
part of `configRevision`, so a spent token never triggers recreation.

The result is a pool that cannot recover from a restart it should survive: the
container lives on durable storage and comes back, the agent process inside it
starts fresh, generates a new key, presents the token baked into its
environment, and is refused because that token was consumed the first time.

This has been invisible because the agent process almost always restarts *with*
its container: the container is recreated when the pool image changes, and that
path mints. Two things make it reachable now. Development images are
content-addressed, so the image changes constantly and hides it. And a
`vz` pool's VM dies with the discobox-server process (ADR 0062) while its
container survives on the data disk, so the agent restarts without its container
on every server restart — as does `wslc`, which persists `/var/lib/docker`.

## Decision

### 1. The identity keypair is durable pool state

The agent persists its keypair and loads it when present. `KeySource` already
describes itself as generating *or loading* the pool identity keypair, so this
is the seam the design anticipated; `GenerateKeySource` becomes the fallback for
first contact rather than the only implementation.

### 2. The key lives in its own tree, mounted from durable storage

`layout` gains a fourth top-level tree beside `projects`, `cache`, and `proxy`,
and `MountRoots` includes it, so every backend provisions it the same way and no
driver learns anything new.

It is a separate tree rather than a file under `PoolData` for two reasons. The
pool-sync reaper and the volume reaper both scan the project and pool subtrees
by design, and a pool's private key must not sit inside anything whose job is to
enumerate and delete. And a sandbox is given its own subtree under the data
tree; a credential that authenticates as the pool belongs nowhere a sandbox path
is derived from.

### 3. Registration is first contact only

An agent that loaded a key does not register. It authenticates with its
assertion, which the control plane already verifies on every other request, and
falls back to bootstrap registration only when the control plane rejects that
assertion — the case where the pool row no longer knows this key.

The bootstrap token keeps its current shape: single-use, short-lived, minted
only when a container is created.

## Consequences

- A pool survives any number of agent restarts without spending a credential.
  The failure this ADR exists to remove — an offline pool that cannot come back
  because its one-time token was consumed before it went away — is gone.
- The steady state stops burning a bootstrap token per agent restart, so the
  token table churns less and `UsedAt` becomes a record of genuine first
  contacts.
- The private key is now at rest. This is the real cost, and it is a widening:
  today an attacker must compromise the running agent, and afterwards it is
  enough to read one file. On a VM backend that file is inside the guest, which
  is the same boundary that already protects the Docker socket the agent holds.
  On the local Docker driver it is on the developer's own filesystem, where the
  pool container's state already lives. The file is written 0600 and its tree is
  never bound into a sandbox.
- Losing the durable tree still forces re-registration, and its token may be
  spent. That case is self-correcting rather than fatal: the container lives on
  the same storage, so losing one loses the other, and `ensurePoolContainer`
  recreates the container with a freshly minted token.
- An agent that starts against a control plane which has forgotten it — a
  restored database, a recreated pool row — falls back to registration and needs
  a live token, exactly as a first contact does.

## Alternatives

**Force container recreation when the VM is new.** The engine already knows
whether `EnsureVM` created one, and passing `forceRecreate` would mint a fresh
token. Rejected: it fixes only the backends that replace a VM, leaves the local
Docker driver's reuse path broken, and keeps spending a single-use credential on
every restart to work around an identity that did not need to change.

**A re-registration endpoint with proof of possession.** Rejected as a second
mechanism for something that already exists: the control plane verifies a pool
assertion on every other agent request, so an agent holding its key is already
provably itself. Adding a parallel proof path would mean two ways to
authenticate the same claim.

**Long-lived or reusable bootstrap tokens.** Rejected: it trades a narrow
recovery problem for a durable credential sitting in container environment,
which is the exact property single-use tokens were chosen to avoid.

**Keep the key in the control plane and hand it back at registration.** Rejected:
the control plane would then hold the pool's private key, and an assertion
signed by it would prove nothing about the agent.

## Deferred: rotating a pool identity

There is no way to rotate a pool's keypair without deleting the pool. It has not
been needed, and the durable key does not make it more urgent than the in-memory
one did — a compromised agent already yielded the key. Revisit when a pool
outlives the trust window of a single key, at which point rotation is a
control-plane-initiated re-key over the existing authenticated channel rather
than another bootstrap token.
