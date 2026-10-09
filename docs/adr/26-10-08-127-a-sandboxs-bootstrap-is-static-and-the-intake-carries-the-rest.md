# 26-10-08-127 — A sandbox's bootstrap is static, and the intake carries the rest

- **Status**: Accepted (refines [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §3: chooses its "the bootstrap carries the config" branch, and says what the
  bootstrap may and may not hold; §2 narrowed by
  [26-10-09-143](26-10-09-143-a-vm-sandbox-starts-its-agent-when-its-bootstrap-arrives.md) for backends with no PID-1 flow)
- **Date**: 2026-10-08
- **Relates to**: [0012](0012-sandbox-config-is-three-attribute-owned-layers.md),
  [0030](0030-pool-agent-polls-and-pushes-sandbox-agent-status.md),
  [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md),
  [0108](0108-a-sandbox-stops-itself-when-its-terminals-go-quiet.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md).

## Context

ADR 0126 §3 moves everything the pool tells a running sandbox onto one
revisioned document, `PUT .../runtime-config`, and allows one exception: a
backend places a bootstrap before the agent exists. It leaves two questions
open. Where a runtime wires volumes before init (a Linux container's PID-1
flow), either the bootstrap carries the config or the flow fetches it first —
pick one. And the bootstrap is "the sandbox's identity, where its pool is, and
the trust material for that hop", which read alone could put the sandbox's
client private key in it.

Driving the intake from the pool also needs an answer neither ADR gives: who
may sign the token that delivers a document. The sandbox agent trusts only the
control plane's key, and the control plane mints pools nothing but
`status:read` (ADR 0030).

## Decision

### 1. Static and dynamic are two kinds of information, with two channels

**Static** is what a sandbox is from the moment it exists until its container
is rebuilt. It is the bootstrap, `sandbox.json`, placed by the backend before
the agent exists and never rewritten — by the pool or by the agent. It holds:

- identity as IDs — project, sandbox, pool;
- public keys to trust — the control plane's, and the pool's (§3);
- where the pool is — the URLs of its egress proxy, credentials endpoint,
  DNS server and BuildKit mediator (§4);
- the create-time effective config the PID-1 flow needs before init — harness,
  declared volumes, sources' layout, user, env, files.

It holds **no private key and no secret**. A change to it is a spec change,
which rebuilds the container with a new one.

**Dynamic** is everything that can change while the sandbox exists, and it
arrives only through the intake: the sandbox's client certificate and private
key, the CAs, the registry namespace, the secret environment (sentinels), the
idle timeout, and each source's origin, pin and delivery state — and with
them readiness. A sandbox that has applied no document has no credentials and
no readiness: its agent serves, and its harness waits.

### 2. The bootstrap carries the create-time config

Of 0126 §3's two branches, the bootstrap carries the config: the PID-1 flow
reads `sandbox.json` and wires volumes and sources before exec'ing init, as it
does today. Every backend therefore has to place one file of a few kilobytes
before boot — a bind on Docker, a seed disk or guest socket for a host VM,
provider user-data elsewhere — which is the cloud-init shape.

### 3. The pool signs the token that delivers a document

The pool's Ed25519 public key is placed in the bootstrap beside the control
plane's (`provider.publicKeys.pool`). The pool signs a sandbox-agent token with
its identity key, and the sandbox agent accepts a token verified by that key
only when it carries exactly the `runtime-config` scope, for this sandbox and
pool, on the runtime-config route. It carries no other authority: a pool-signed
token cannot read a terminal or start an exec.

This adds no trust the bootstrap did not already imply: whoever could forge the
pool's key could already write `sandbox.json`, the control plane's key in it
included.

### 4. A bridge is the pool's endpoint, the sandbox's listener, and the delivered credential

- **Where the pool is** — each bridge's upstream URL, the credentials URL, the
  DNS server — is static and in the bootstrap. Only the pool knows it.
- **Where the sandbox listens** — the egress forwarder, the BuildKit forwarder,
  the DNS stub's link-local address — is the sandbox's own, a shared constant
  (`sandboxconfig`). The pool reads the same constants for the two settings it
  makes that must agree with them: the proxy env in the bootstrap, and the
  DNS server Docker hands the container. The nested-Docker listener stays
  discovered at runtime.
- **The credential** — keypair and CAs — is dynamic.

The sandbox agent renders its bridge configs from the three. The document
carries no bridge.

### 5. The sandbox acts on what it applies

A delivery that changes the proxy material starts or restarts the units that
read it — the trust-store unit and the bridges — before readiness is
published, so a harness never launches with an egress hop that is not up. The
idle timeout is applied to the running idle-stop policy when it is delivered,
not on the next start.

### 6. Every pool sandbox's first launch waits for its first document

The readiness gate holds the first harness launch and the declared services of
every sandbox whose bootstrap names a pool key — every sandbox a pool delivers
to — until an applied document grants readiness, not only a sandbox awaiting a
pushed source. sandbox-agent itself is never held.

### 7. An agent without the intake is refused, not staged around

A sandbox whose image predates the intake answers its route with 404. The pool
fails the start with an error that says the image must be rebuilt on a current
base, rather than falling back to staging files.

## Alternatives rejected

- **A minimal bootstrap, with the PID-1 flow waiting for the config over the
  connection.** Smaller per-backend bootstrap, but the document would grow to
  carry the create-time config, the PID-1 flow would have to serve a request
  before init exists, and a pull from the pool would reverse ADR 0030's
  direction. Nothing it buys is needed by a backend we run today; it can follow
  without undoing this.
- **Bootstrap the proxy keypair** (copy the material in at create). It starts
  the bridges on the first boot with no unit changes, and puts a private key in
  the bootstrap — the one thing §1 keeps out of it.
- **The control plane mints runtime-config tokens.** No new key in the sandbox,
  but every delivery — create, start, secret rotation — needs a control-plane
  round trip, and what a sandbox is told is the pool's to say.
- **The pool decides the sandbox's listen addresses** (as the document from
  #44 did). Where a sandbox listens is not knowable from outside every backend
  and is not the pool's business; carrying it made the bridge a pool-shaped
  file.
- **A staging fallback for images without the intake.** Two mechanisms for one
  job, which is what 0126 §1 rejects.

## Consequences

- `sandbox.json` gains the pool's key and endpoints and loses
  `agentRuntime.idleTimeout`; the runtime-config document loses its bridges.
- The pool keeps its own record of what it has decided — the document, its
  revision, and the project layer it built the bootstrap from — rather than
  reading any of it back from a file the sandbox can write.
- On Docker the config and secrets binds become writable inside the sandbox,
  since the agent is now what writes them.
- Live certificate renewal (#62) is a new document, which §5 already acts on.
