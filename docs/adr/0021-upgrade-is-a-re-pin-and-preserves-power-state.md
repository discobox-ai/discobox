# 0021 — Upgrade is a desired-state re-pin, and replacing a container preserves its power state

- **Status**: Accepted
- **Date**: 2026-08-06
- **Supersedes**: [ADR 0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md)
  §§4–5. §§1–3 and §§6–8 stand.

## Context

[ADR 0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) made image
upgrades explicit and digest-driven, with two exceptions to "explicit": §4 made
upgrade a re-pin *plus a restart*, and §5 let a sandbox that was not live re-pin
itself to its harness config's current image on its way up.

Two things have changed under those sections.

[ADR 0017](0017-resource-state-is-desired-and-observed-with-no-operations.md)
replaced operations with generation convergence. The `RestartGeneration` bump
§4 relied on no longer exists: `UpgradeSandbox` records intent, and the
reconciler delivers the new spec through the ordinary `provider.Create` path,
where §5 of that ADR already detects drift by spec fingerprint. Nothing needs a
restart to "make the runtime look".

ADR 0017 §12 then put an auto-start latch in the pool agent: sandbox-directed
traffic starts a stopped sandbox without consulting the control plane. That is
the common way a stopped sandbox comes up, and it runs no reconcile — so the §5
re-pin does not fire on it. What §5 describes as a policy ("a stopped sandbox
upgrades itself on its next start") is in practice conditional on something else
having marked the sandbox dirty around the same time. A rule that upgrades a
sandbox on some starts and not others is worse than either answer.

A third problem is independent of both: the reconciler passes `Start` only on a
first create (ADR 0017 §13), so today the pool agent replaces a **running**
sandbox's container for a spec change and leaves the replacement stopped. An
upgrade silently powers the sandbox off.

## Decision

### 1. Upgrade writes desired state and nothing else

`POST …/sandboxes/{id}/upgrade` re-pins `Image` and `ImageDigest`, bumps the
generation, and marks the sandbox dirty, in one transaction. That is the whole
operation. Delivery is the ordinary reconcile: the new spec reaches the pool
agent as a create request, whose fingerprint no longer matches the running
container's label, and the pool agent converges it (ADR 0017 §5).

This is what ADR 0016 §4 was reaching for with "changing the pin *is* the
instruction". The restart it paired with the re-pin was a way to make a runtime
that only looked at creates notice; convergence removed the need.

### 2. No implicit re-pin, ever

A sandbox runs the image it is pinned to until somebody upgrades it. ADR 0016 §5
is withdrawn, and with it the reconciler's re-pin of any sandbox that is not
live.

The reason is predictability, not safety. Under §5 the pin advanced on a
reconcile of a non-live sandbox — an event a user does not issue and cannot see.
Whether the sandbox they come back to is the image they left depends on whether
something marked it dirty while it was stopped. Since ADR 0017 §12, the ordinary
way a stopped sandbox comes up does not reconcile at all, so the rule fires
mostly by accident.

The cost is real and accepted, and it is narrower than "a stopped sandbox is
stranded". Starting a sandbox whose container still exists resolves no image at
all — the pool agent starts the container it already has, and Docker will not
prune an image that container references. The exposure is a sandbox whose
container is *gone* (removed, recovered, or a rebuilt pool) and whose pinned
digest is no longer on that pool: the create fails in `resolveSandboxImage`, the
reconciler records the failure, and the sandbox reads as failed with the
runtime's own message naming the fix — *upgrade the sandbox to move it to the
current image*. An explicit failure pointing at a one-word command is a better
answer than a silent image change.

Upgrade availability stays derived and stays reported (ADR 0016 §2), so a client
can always see that a sandbox is behind its harness config.

### 3. Replacing a container preserves the power state it found

When the pool agent replaces a sandbox container because its spec drifted, it
records whether that container was running, stops and removes it, creates the
replacement from the new spec, and starts the replacement **only if the one it
replaced was running**.

- Upgrading a running sandbox is a restart into the new image. Its terminals end
  and the primary terminal relaunches under the harness's relaunch command.
- Upgrading a stopped sandbox leaves it stopped. The container is rebuilt so the
  next start is an ordinary start, but an upgrade never powers a sandbox on.

The container is replaced immediately rather than deferred to the next start:
deferring would leave the pin and the container disagreeing for an unbounded
time, which is the state ADR 0017 §5's fingerprint check exists to eliminate.

### 4. `Start` in a create request is first-create intent, not "ensure running"

The `Start` flag means "no container existed and one is being created for the
first time, bring it up" (ADR 0017 §13). It does not describe the desired power
state of a sandbox that already exists, and the runtime must not read its
absence as "stop this sandbox": the replacement comes up if the container it
replaced was running **or** the request asked for a first-create start. The two
inputs only ever add a start; neither can take one away.

The control plane is deliberately not asked to compute this. Its view of power
state is an observation that can be stale by the time the request lands, while
the pool agent holds the only authoritative answer and already serializes
against concurrent starts and stops on its own mutex (ADR 0017 §12).

### 5. Only the pool agent can say whether an image can be run

Whether a pinned digest is present on a pool, or can be pulled onto it, is a
fact about that pool's image store and its network — not about the sandbox
record. The pool agent is the only component that can determine it, and it does
so at exactly one moment: resolving the image while building a container
(`resolveSandboxImage`). The control plane pins a digest; it never predicts
whether that digest can be obtained, and it must not try.

Two rules follow.

The server does not gate on image availability. An upgrade is refused only when
there is nothing to move to (ADR 0016 §2) — never because the target "might not
be pullable", which the server cannot know. The harness config's digest is a
control-plane fact about *identity*; it says nothing about any pool's ability to
run it, and the two must not be conflated because both are digests.

An image that cannot be resolved is reported, not predicted. The pool agent's
create fails, that failure travels back as observed state, and the sandbox reads
as failed carrying the agent's own message, over the reporting channel that is
already the way runtime facts travel (ADR 0017 §10). A runtime fact reaches the
user as an observation from the component that observed it, never as a
control-plane guess made in advance.

This is also what makes the withdrawal of ADR 0016 §5 in §2 above coherent.
Auto re-pinning was a control-plane rescue for a runtime condition the control
plane cannot detect: it moved every non-live sandbox onto a new image on the
chance that the pinned one had become unrunnable, without ever knowing whether
it had. The pool agent already reports the real condition when it happens.

## Alternatives rejected

**Keep ADR 0016 §5, and make it fire on the §12 auto-start path too.** This
makes the rule uniform, at the price of making every attach to a stopped sandbox
a potential image change. The user-visible behavior — "my sandbox was replaced
because I attached to it" — is the same surprise §4 refused to allow at the
running end. Uniformity is the wrong axis to fix here; explicitness is.

**Upgrade as its own operation, with its own counter pair.** Rejected for the
reason ADR 0016 §4 gave and this ADR keeps: it is a second way to say "converge
this sandbox", with a second reconciler branch to keep equivalent to the first.

**Have the server pass the desired power state on the create request.** The
server would have to read its last observation of the sandbox's state and send
it as intent, reintroducing exactly the observation-as-intent confusion ADR 0016
§8 and ADR 0017 warn about, and racing a start or stop that lands in between.
The pool agent already knows.

**Leave the old container in place until the next start.** Cheaper for a stopped
sandbox, but it splits "what this sandbox is" between the pin and the container
for an unbounded window, and reintroduces per-start upgrade work — the thing §2
just removed.

## Consequences

- `UpgradeSandbox` remains refused when the sandbox is already on its harness
  config's current digest (ADR 0016 §2): the recreate costs the container
  filesystem outside the volumes and buys nothing.
- Upgrading a running sandbox ends its sessions. That cost is unchanged from
  ADR 0016 §4 and must be stated at the point of opt-in.
- A stopped sandbox never changes image on its own, so a long-stopped sandbox
  can be arbitrarily far behind its harness config. Pruning a pool's images
  strands only the sandboxes whose containers are also gone, since a start
  resolves no image. Both are visible: availability is derived and reported, and
  a sandbox that cannot be rebuilt reads as failed with a message naming the
  upgrade.
- In development, where the harness image tag is rebuilt continuously, every
  sandbox stays on the image it was created with until upgraded. This is the
  intended behavior, not friction to design around.
