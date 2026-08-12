# 0031 — Agent credentials are a portable list/request/get protocol with pool-agent-minted ephemeral sentinels

- **Status**: Proposed
- **Date**: 2026-08-11

## Context

An agent working inside a sandbox sometimes needs a credential it was not
provisioned with: a GitHub token to open a PR, an API key for a service the
task touches. Today the only paths into the secret system are proactive
(harness-config bindings and `--secret` flags at sandbox create) or reactive
(the proxy hits an unresolvable sentinel and auto-mints a bare
`SecretRequest`). There is no way for the agent to *ask* — to say what it
needs, why, and what it intends to do with it — and then use the answer.

Discobot solved this with tools built into its agent: `RequestUserCredential`
pauses the turn for user approval and returns opaque session-scoped ids, and
`Bash` takes a `credentialUses` parameter that injects the real value into the
child process environment after static checks plus an in-agent LLM judging the
command against approved-use descriptions. That design works there because
discobot's trust boundary includes the in-sandbox agent process, which holds
the decrypted values in memory anyway. Ours does not: the discobox sandbox is
untrusted by construction, the real value never enters it, and the proxy swaps
a sentinel for the value on outbound requests only while a live `SecretGrant`
covers the sandbox and destination host.

We want the discobot capability — agent requests, human approves, agent uses —
without importing its trust model, and without coupling it to any one harness:
the same mechanism must work for Claude Code, or any other harness that can run
a CLI.

## Decision

**Agent credential access is a small versioned protocol — list, request, get —
served over a URL. A CLI in the sandbox image is a thin client of that
protocol. In discobox, sandbox-agent implements the protocol and relays over
its issued mTLS identity to the pool-agent, which mints short-lived
"ephemeral" sentinels per use; grants and cleartext stay on the control
plane. Elsewhere, anything that implements the protocol can serve real values
directly.**

### 1. The protocol is portable and knows nothing about discobox

Three operations, versioned under `/v1`, specced as their own document:

- **list** — granted credentials and their approved uses (`useId`,
  description, expiry). Never values.
- **request** — `{name, envVar, host, justification, uses: [{description}]}`
  returns a `requestId` immediately; status is polled until
  `granted`/`denied`. Approval is human-latency, so the request primitive is
  asynchronous; blocking UX (`--wait`) is the CLI's, built on polling.
- **get** — `{useId, command}` returns `{envVar, value, expiresAt}`. The
  caller declares the command it is about to run; the value it receives is a
  sentinel in discobox and may be a real secret in other implementations.

The CLI is configured with a base URL (and optional bearer token for
non-discobox implementations) and has no discobox-specific behavior. Its
best-ergonomics form is a wrapper — `run --use <useId> -- <command>` — which
makes the declared command literally the argv it executes and injects the
returned value into only that child process's environment.

### 2. In discobox, the trusted side makes every decision

The implementation chain is: CLI → sandbox-agent (localhost, same trust
posture as the hooks socket) → pool-agent over the existing mTLS bridge,
where the sandbox client certificate is the identity → control plane. The
sandbox never holds a control-plane credential and never participates in an
authorization decision. The declared command is recorded on the trusted side
*before* use; enforcement happens against the actual outbound traffic at swap
time, so the declaration is narrowing context and audit trail, never a trust
anchor.

### 3. Two TTLs, two owners

- **Grant TTL — control plane, consent clock.** `SecretGrant.ExpiresAt` as
  today ("for a day"). Cleartext only ever leaves the control plane through
  `ResolveSandboxSecret`, which checks grant liveness per resolve, and the
  proxy's positive cache TTL is capped at grant expiry — so once the grant
  lapses, nothing on the pool host can obtain or keep serving the value.
- **Sentinel TTL — pool-agent, use clock.** `get` causes the pool-agent to
  mint a fresh ephemeral sentinel (using the secret's `Format` so it
  byte-mimics a real key), record the activation
  `{ephemeral sentinel → stable binding, useId, command, expiresAt}` locally,
  and register it with the proxy. At swap time the resolver validates the
  activation (alive, host consistent with the use), translates ephemeral →
  stable sentinel, and calls `ResolveSandboxSecret` unchanged. The control
  plane never learns ephemeral sentinels exist.

`get` returns `expiresAt` so the agent knows the window and re-gets instead of
hoarding. Pool-agent restart drops activations; the failure mode is a dead
sentinel and a fresh `get` — fail closed.

### 4. Credentials granted through this flow have no static binding

Unlike harness-config secrets, a CLI-requested credential is never written
into the sandbox env or `secrets.json`. The sentinel exists only inside an
activation window, injected by the CLI into the wrapped process. A leaked
ephemeral sentinel dies in minutes at the pool-agent; a leaked stable sentinel
dies at grant expiry on the control plane.

### 5. Augment `SecretRequest`/`SecretGrant`; host becomes mandatory here

- `SecretRequest` gains `envName`, `justification`, and requested
  `uses []{description}` alongside its existing `Type`, `Host`, `SandboxID`.
- `SecretGrant` gains approved uses `[]{useId, description}`, confirmed or
  edited at approval time.
- The approve flow (`disco secret request approve` / the API) is the human
  surface, unchanged in shape; approving a protocol-originated request
  additionally resolves the requester's poll.
- A grant minted by approving a protocol request **must** carry a concrete
  host. `FindLiveGrant` already matches the actual destination host observed
  by the proxy, so a token cannot be swapped toward an unapproved domain; the
  wildcard (`host: ""`) grant remains an explicit administrative act via
  `disco secret grant create`, never a product of this flow. The activation
  check repeats the host test locally at the pool-agent before calling home.
- Reactive requests (proxy hits an unresolvable sentinel) remain a distinct,
  bare species; the approve handler treats requests with and without declared
  uses differently rather than pretending they are one shape.

### 6. Semantic (LLM) use-judging is deferred, behind the protocol

Judging the actual request against approved-use descriptions with a model is
an enforcement enhancement inside `ResolveSandboxSecret` (or the resolver
path), using a project-designated LLM secret the control plane already holds.
The protocol, CLI, and pool-agent are unaware of it. Revisit when host + TTL +
activation scoping proves too coarse — the request vocabulary (justification,
use descriptions) is captured from day one so the judge can slot in without a
protocol change. It must never run in the sandbox.

## Alternatives rejected

**Built-in agent tools, as discobot does.** Ship request/use as tools inside
the harness. Rejected because it couples the capability to one harness's tool
surface; a CLI speaking a protocol works from any harness, any script, or a
human at a shell, and the harness only needs to be told the CLI exists.

**In-sandbox authorization, as discobot does.** Let the agent runtime hold
values and judge uses (with an LLM) before injecting them. Rejected because
our sandbox is untrusted: a prompt-injected or compromised agent fabricates
the verdict or skips the check. Discobot also enforces against the *claimed*
command and trusts its behavior matches; our enforcement point is the proxy,
which sees the actual traffic. Any port of the in-sandbox model would be
strictly weaker than what the sentinel design already provides.

**Server-managed ephemeral sentinels.** Have the control plane mint and track
per-use sentinels. Rejected: activations are minutes-scale, high-churn,
per-sandbox state, and the pool-agent already owns the sentinel registry and
the resolver the proxy calls. Keeping them local avoids a control-plane
round-trip on `get`, keeps `ResolveSandboxSecret` and `SandboxSecret`
unchanged, and loses nothing — activation state is safely disposable (§3).

**Return real values from `get` in discobox.** Simplest possible CLI story
and what discobot effectively does. Rejected: it breaks the containment
invariant that the sandbox never holds cleartext, and discards the swap-time
enforcement (host scope, grant liveness, future judging) that is the point of
the proxy. The protocol *allows* real values so other implementations can be
simple; discobox deliberately does not use that freedom.

**Static env binding with an always-live sentinel (today's harness-secret
shape).** Bind the granted credential into the sandbox env like harness
secrets. Rejected for this flow: an always-present sentinel is always worth
stealing and always resolvable while the grant lives. Ephemeral-only makes
the credential inert by default and gives `get` a meaning — every use is an
attributable, windowed, declared event.

**A synchronous `request` that blocks until approval.** Rejected as a protocol
primitive: approval can take minutes or never come, and long-held connections
through CLI → sandbox-agent → pool-agent are fragile. Poll status; let the CLI
own blocking UX.

**Ship the swap-time LLM judge in v1.** Rejected for now (§6): host + TTL +
activation scoping is deterministic and already enforced twice on the trusted
side; the judge adds latency, a model dependency, and a consent question
(whose tokens) that deserve their own decision once the coarse controls prove
insufficient.

## Consequences

- A new protocol spec document (list/request/get, `/v1`) lands with the
  implementation; it is the contract for the CLI and for non-discobox
  implementations.
- The CLI ships in the sandbox image alongside `discobox-sandbox-agent`
  (argv[0] dispatch, like `discobox-hook-publish`), configured with the
  sandbox-agent's localhost URL.
- sandbox-agent gains the three protocol routes; pool-agent gains their
  relayed implementations, the activation store, ephemeral-sentinel minting,
  and the activation check in its resolver.
- `model.go`: `SecretRequest` gains `EnvName`, `Justification`, and requested
  uses; `SecretGrant` gains approved uses. Approve handling distinguishes
  protocol-originated from reactive requests and rejects hostless grants for
  the former.
- The proxy's positive resolution cache is capped at grant expiry universally
  (today only the OAuth path caps it).
- Only bearer/oauth secrets are usable through this flow for now —
  `ResolveSandboxSecret` emits `Value.Token` alone, so git/ssh secrets cannot
  be proxy-swapped; extending the resolver is separate work.
- `SecretRequest`/`SecretGrant` events already stream over project SSE, which
  is what makes `--wait` and approval UIs cheap.
- The declared command plus activation records give the audit log a per-use
  story: which command, which use, which requests, over what window.
