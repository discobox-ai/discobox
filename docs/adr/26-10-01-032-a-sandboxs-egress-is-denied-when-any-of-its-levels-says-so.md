# 26-10-01-032 — A sandbox's egress is denied when any of its levels says so

- **Status**: Accepted
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
  reaches out freely", and the identity a pool's own BuildKit fetches under;
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
fields. The built-in harnesses declare their model APIs. A person's harness
level `allow` is kept separately and is not overwritten by a re-registration.
Declaring hosts never restricts anything (§2).

The project's judge is an ordinary discobox on the project's default harness,
so a restricted project's judge reaches the hosts that harness declares and
no special case is needed for it.

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

### 5. Delivery: pushed, and converged by the reconciler

The effective policy is computed on the server and is not in the sandbox
manifest: it changes live, and a manifest change rebuilds the container
(`SandboxManifest.Fingerprint`). It travels as sentinels do —
`PoolSandboxCreateRequest` and `PoolSandboxUpdateRequest` carry it, the pool
agent records it beside them, and the proxy unit's policy publisher adds it as
a fourth input to every `ApplyConfig`, so the policy is in force before the
sandbox's first packet.

A change at any level marks every sandbox it affects for reconcile; the
sandbox reconciler pushes a running sandbox's effective policy, and a stopped
one gets it at its next create. Tightening takes effect on the next request,
not a poll interval later.

A project or pool change also changes the policy of the pool's own client
(§7). The pool reconciler pushes it to the pool agent, which the project or
pool change marks for reconcile as well.

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
the pool registry, and the pool agent's own traffic. buildkitd's own fetches
are not outside it; §7 says what governs them. A restricted sandbox still
reaches the Discobox API it was given.

### 7. A build's own fetches follow the project and pool levels only

A pool's shared BuildKit makes two kinds of connection
([ADR 0044](0044-builds-run-on-a-pool-shared-buildkit.md) §6):

| Traffic | Identity at the proxy | Policy applied |
| --- | --- | --- |
| `RUN` steps, through the per-build forwarder | the sandbox (the forwarder holds its certificate) | the sandbox's effective policy, as its own traffic |
| buildkitd's own: `FROM` pulls, `ADD <url>`, git contexts, cache import | the pool | the **project and pool** levels alone |

buildkitd fetches in no container and under no sandbox's identity, so the
proxy cannot tell whose build a pull is for, and a harness or sandbox level
cannot reach it. A pool belongs to one project, so the project's and the
pool's levels are known for every build on it, and §2's rule over just those
two is the one policy those builds share: buildkitd's client is restricted
when either sets `denyByDefault`, to the union of their `allow`.

**To restrict what builds may pull, set it at the project or pool level.** A
harness or sandbox `denyByDefault` restricts that sandbox's `RUN` steps but not
the base images its builds pull, and a host only a harness or sandbox allows is
reachable from a `RUN` step but not by a `FROM` in a restricted pool. Image
registries a pool's builds use belong in its project's or its own `allow`.

Attributing buildkitd's fetches to the sandbox whose build they serve is
[ADR 26-10-01-874](26-10-01-874-a-builds-registry-credentials-are-sandbox-sentinels.md),
proposed and not part of this decision.

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
- **Put the policy in the sandbox manifest.** Every change would rebuild the
  container.
- **No pool level.** The project covers a project with one pool, but not a
  pool on a network the rest of the project is not on, and buildkitd's
  identity is the pool's: without a pool level, builds could be restricted
  only project-wide.
- **Exempt the judge.** Unneeded once harness images declare their hosts (§3),
  and an exemption is a hole in a project's restriction.

## Consequences

- A restricted project's or pool's builds pull only from hosts the project and
  pool allow (§7).
- A domain-only allow list refuses a request to an IP literal, including a
  SOCKS client that resolves locally: an IP is matched only against IP and
  CIDR entries.
- In-sandbox `git fetch`/`push` to a remote origin, package registries, and
  BuildKit `RUN` steps (filtered as the owning sandbox) need their hosts
  allowed in a restricted sandbox.
- A host the filter denies cannot be trusted either (`ProbeTLS` refuses it).
- A discobox created by a restricted one stays restricted.
- DNS remains open (§8).
