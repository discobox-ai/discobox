# 26-10-01-032 — A sandbox's egress is denied when any of its levels says so

- **Status**: Accepted (narrows [26-10-14-320](26-10-14-320-a-discobox-acts-on-any-discobox-in-its-project-and-the-judge-decides-which.md) §§1, 3)
- **Date**: 2026-10-01
- **Relates to**: [ADR 0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md)
  §6, which makes the pool proxy a sandbox's only route out;
  [ADR 0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md),
  whose sandbox role may create discoboxes;
  [ADR 0148](0148-a-sandbox-resolves-names-through-its-pool-over-mtls.md),
  which deferred filtering DNS.

## Context

Every byte a sandbox sends off its pool passes through the pool proxy, and the
proxy has a destination filter (`proxy/internal/filter`: domains, `*.`
wildcards, IPs, CIDRs). Nothing in Discobox turns it on. A person cannot say
"this discobox may reach only GitHub and the model API", for one discobox or
for a whole project.

The filter as it stands cannot say it either. `Enabled` is one switch for the
whole pool: on, every client is denied except what a global list or a rule
naming it allows. A pool hosts discoboxes from many settings at once, so "this
one restricted, that one open" has no shape in it.

Policy has four natural owners:

- the **project** — "nothing in this project talks to the internet freely";
- the **pool** — "this pool sits beside internal services; nothing in it
  reaches out freely";
- the **harness config** — "Claude Code needs `api.anthropic.com`";
- the **sandbox** — "this one also needs `registry.npmjs.org`".

A harness's hosts are a property of the harness, not a decision to restrict
it. If an allow entry implied deny-by-default, a harness that shipped its
hosts would lock down every discobox using it. So restriction and the hosts
it admits are separate settings.

## Decision

**Each of project, pool, harness config, and sandbox carries an egress
policy: a list of allowed destinations and a `denyByDefault` flag. A sandbox
is restricted when any of its four levels sets `denyByDefault`. A restricted
sandbox reaches exactly the union of the four lists, plus the hosts its
harness image declares; an unrestricted one reaches everything, and the lists
have no effect. The server computes the effective policy and pushes it to the
pool, whose proxy enforces it per sandbox.**

### 1. Shape

```yaml
EgressPolicy:
  allow: [string]       # "github.com", "*.googleapis.com", "10.0.0.0/8", "1.2.3.4"
  denyByDefault: bool
```

On `Project`, `Pool`, `HarnessConfig`, and `Sandbox`, each optional, absent
meaning `{allow: [], denyByDefault: false}`. An entry is a domain pattern (as
`rules.MatchDomain` reads it), an IP, or a CIDR — the filter's own
vocabulary, host only, no port. Entries are validated when written, not when
enforced.

### 2. Merge

```
restricted = project.denyByDefault || pool.denyByDefault
          || harness.denyByDefault  || sandbox.denyByDefault
allow      = project.allow ∪ pool.allow ∪ harness.allow ∪ image.egressHosts ∪ sandbox.allow
effective  = restricted ? only(allow) : everything
```

`restricted` with an empty `allow` denies everything. No level can narrow
another: a level adds hosts or turns restriction on, and nothing turns it off
below where it was set. That is what lets a project's `denyByDefault` stand
whatever a pool, a harness, or a sandbox says. The levels are listed
project → pool → harness → sandbox, broadest first, but the order decides
nothing: the rule is symmetric.

### 3. A harness image declares the hosts it needs

`harness.Image` (the `io.discobox.harness.v1` label) gains `egressHosts`,
snapshotted into the harness config at registration like its other declared
fields. A person's harness level `allow` is kept separately and is not
overwritten by a re-registration. Declaring hosts never restricts anything
(§2).

An image declares only what its agent needs to run, from its vendor's own
documentation. `claude-code` declares the hosts Anthropic's network
documentation lists for the API, sign-in, token refresh, and claude.ai
connectors. `codex` and `opencode` declare none until theirs are established
the same way; until then a restricted project lists their model hosts at a
level itself, and a restricted codex or opencode discobox reaches none of them
otherwise.

The project's judge is an ordinary discobox on the project's default harness,
so a restricted project's judge reaches the hosts that harness declares and
those the levels allow, and no special case is made for it.

### 4. Who may set what

- Project, pool, and harness config: their `PATCH` routes, as their other
  settings. The sandbox role (ADR 0140) is already refused all three.
- Sandbox: on create and through `PATCH` of the sandbox config. The sandbox
  role is already refused the update.
- **A discobox may not create one wider than itself.** A create from the
  sandbox role computes the new sandbox's effective policy — the project, the
  chosen pool, the chosen harness, and the body's own level — and refuses it
  unless it is covered by the creator's: if the creator is restricted, the
  child must be, and each entry of the child's `allow` must be covered by an
  entry of the creator's (a domain by an equal or wider pattern, an IP or CIDR
  by a CIDR containing it). Checked at create only; later edits are a person's.
- **A discobox may not act on one wider than itself.** The sandbox role also
  reaches existing discoboxes in its project (ADR 26-10-14-320): typing into
  their terminals, HTTP to their ports, powering them, running their
  services, writing their meta, delivering their source. Each of those writes
  to or sends into the target, so a restricted discobox could reach anything
  through an unrestricted sibling. Every such route is allowed only when the
  target's effective policy is covered by the caller's, by the same rule as a
  create, read as both stand at the call. Reads — a discobox, its terminals'
  screens and transcripts, its services and logs — bring data to the caller,
  never out of it, and are not held to it.

### 5. Delivery: carried by the create, then pushed until it lands

The effective policy is computed on the server whenever it is delivered, and
is not in the sandbox manifest: it changes live, and a manifest change
rebuilds the container (`SandboxManifest.Fingerprint`).

- **A create carries it.** `PoolSandboxCreateRequest.egress` is recorded at
  the pool before the container is built, and the proxy unit's policy
  publisher adds it as a fourth input to every `ApplyConfig`, so a new
  sandbox is restricted from its first request.
- **Every change after that is pushed on its own.** A change at any level
  marks the egress delivery of every sandbox it affects that is meant to
  exist, in the transaction that writes the level. That delivery is its own
  reconcile, not the sandbox's: it merges the policy and sends it alone on
  `PoolSandboxUpdateRequest.egress`, which the pool records for a sandbox
  whose container exists; one not yet built gets the policy from its create. It retries with the engine's backoff while a pool is
  unreachable, records nothing on the sandbox, and reaches one whatever its
  lifecycle is doing — a settled failure included, whose container may still
  be running.
- **A create is always followed by a push.** A create merges its policy when
  it runs, and a level change committed after that pushes a policy the pool
  may take before the create lands. The sandbox reconciler marks the delivery
  after every create attempt, failed ones included — the pool records the
  create's policy before anything that can fail — so the last write is always
  a push merged after it.
- **A sandbox's own level is written by itself.** It is not spec and bumps no
  generation, so the sandbox reconciler's generation-guarded row write never
  writes it, or a reconcile that loaded the row first would put the old level
  back.

### 6. The filter is per client

`filter.Config` loses its pool-wide `Enabled` and its client-scoped rules, and
holds one policy per client ID (the mTLS CN, the sandbox ID): restricted with
an allow list, or absent. An absent client is unrestricted, as every client is
today. `proxy.AllowlistConfig` follows. Nothing outside tests sets the old
fields, so there is nothing to migrate. Denials stay as they are: `403` for
HTTP, a refused `CONNECT` or SOCKS connect, audited `host denied`.

What is outside the filter stays outside it: the gate host
(`api.discobox.internal`, intercepted before the filter), the credential
broker, the DNS forwarder, the hop from a sandbox to the BuildKit mediator,
the pool registry, the pool agent's own traffic, and buildkitd's own fetches
(§7). A restricted sandbox still reaches the Discobox API it was given.

### 7. A build's own fetches are not filtered yet

A pool's shared BuildKit makes two kinds of connection
([ADR 0044](0044-builds-run-on-a-pool-shared-buildkit.md) §6):

| Traffic | Route | Policy applied |
| --- | --- | --- |
| `RUN` steps | the per-build forwarder, to the pool proxy, as the sandbox | the sandbox's effective policy, as its own traffic |
| buildkitd's own: `FROM` pulls, `ADD <url>`, git contexts, cache import | the pool container's own network, **not** the pool proxy | none |

buildkitd is given the pool process's proxy environment, and a pool is given
one only when it runs nested inside a discobox — then it is the outer box's
proxy, not its own. Otherwise buildkitd dials origins directly. So a
restricted sandbox's `docker build` can pull any base image and `ADD` any URL;
only its `RUN` steps are held to its policy.

This is recorded rather than fixed here. Routing buildkitd through the pool
proxy, under an identity of the pool's own, would let the project and pool
levels — the two known for every build on a pool, since a pool belongs to one
project — govern it; attributing its fetches to the sandbox whose build they
serve is [ADR 26-10-01-874](26-10-01-874-a-builds-registry-credentials-are-sandbox-sentinels.md).
Revisit when a restricted sandbox's builds must be held to its policy, or
when 26-10-01-874 is taken up, whichever is first: the routing is a
prerequisite of both.

### 8. DNS is not filtered yet

A restricted sandbox can still resolve any name, and DNS is an exfiltration
channel. ADR 0148 deferred filtering it, and this keeps it deferred:
revisit when a restricted sandbox is expected to hold a credential worth
exfiltrating byte-at-a-time, or when someone asks for egress to be a
containment boundary rather than a policy. The forwarder already knows the
sandbox from its certificate and could ask the same filter.

## Alternatives rejected

- **An allow entry implies deny-by-default.** The first version of this
  decision. A harness could not declare the hosts it needs without
  restricting every discobox that used it, so restriction became its own flag.
- **The narrowest level wins, or a lower level overrides.** Lets a sandbox
  undo a project's restriction, which is what a project-level setting exists
  to prevent.
- **The pool proxy polls a per-pool route, as host trusts do.** No fan-out
  needed, but a new sandbox would run unrestricted until the first poll, and
  tightening would take up to the interval.
- **Deliver changes through the sandbox's own ensure.** The first version of
  §5. Ensure skips a settled failure, so a failed sandbox whose container
  still ran kept the old policy; and an ensure against an unreachable pool
  waits out the pool and records the sandbox failed, so a policy edit during
  an outage broke every healthy sandbox on that pool.
- **Treat a level change as intent on each sandbox.** Reaches a failed
  sandbox, but still records a failure on every sandbox of a pool that is down
  when the policy changes.
- **Put the policy in the sandbox manifest.** Every change would rebuild the
  container.
- **No pool level.** The project covers a project with one pool, but not a
  pool on a network the rest of the project is not on.
- **Leave which discobox a call may act on to the judge** (ADR 26-10-14-320
  §§1, 5). The judge reads what a call is for; it cannot know what reaching
  a wider discobox means to a policy it was never shown, and a judged call is
  still one judged call — typing a command into an unrestricted sibling is one
  call that sends anything anywhere. The bound is a fact the control plane
  holds, so it is checked there, before the judge is asked.
- **Exempt the judge.** A judge on a harness that declares its hosts needs
  nothing more (§3), one on a harness that declares none is in the same place
  as every other discobox on it, and an exemption is a hole in a project's
  restriction.

## Consequences

- A restricted sandbox's builds still pull base images and `ADD` URLs
  unfiltered (§7).
- A domain-only allow list refuses a request to an IP literal, including a
  SOCKS client that resolves locally: an IP is matched only against IP and
  CIDR entries.
- In-sandbox `git fetch`/`push` to a remote origin, package registries, and
  BuildKit `RUN` steps (filtered as the owning sandbox) need their hosts
  allowed in a restricted sandbox.
- A host the filter denies cannot be trusted either (`ProbeTLS` refuses it).
- A discobox created by a restricted one stays restricted, and a restricted
  one types into, posts to, or powers only discoboxes within its own policy.
- DNS remains open (§8).
