# 26-10-27-880 — Access to a secret is a binding held to static routes, and no judge decides it

- **Status**: Accepted (§4's hosts, and §8's migration of a secret's host, of an uncovered harness binding and of several grants on one variable, narrowed by [26-10-27-939](26-10-27-939-a-route-may-name-any-host.md); §2's levels narrowed by [26-10-27-172](26-10-27-172-a-harness-config-attached-to-a-discobox-binds-it-for-use.md))
- **Date**: 2026-10-27
- **Supersedes**:
  - Wholly, every decision whose authority is a judge, Jev, an approved-use
    sentence, a policy of statements, or an approval rule:
    [0079](0079-a-local-judge-gates-every-wrapped-credential-use.md),
    [0090](0090-the-judge-is-handed-facts-and-given-no-tools.md),
    [0091](0091-a-credential-is-not-issued-without-a-verdict-on-record.md),
    [0092](0092-the-cli-has-no-unjudged-way-to-take-a-value.md),
    [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md),
    [26-09-25-428](26-09-25-428-the-judge-may-let-its-allow-stand-for-a-route.md),
    [26-09-26-240](26-09-26-240-a-request-is-recognized-and-its-body-is-shown-in-one-shape.md),
    [26-09-27-905](26-09-27-905-the-command-judge-is-shown-a-bounded-stdin.md),
    [26-10-01-324](26-10-01-324-a-server-may-judge-with-jev-instead-of-a-judge-discobox.md),
    [26-10-02-054](26-10-02-054-commands-are-judged-by-default-and-requests-by-opt-in.md),
    [26-10-07-640](26-10-07-640-jevs-claim-of-approval-hazard-is-told-what-its-evidence-is.md),
    [26-10-07-937](26-10-07-937-every-jev-refusal-goes-to-the-judge-discobox.md),
    [26-10-12-330](26-10-12-330-a-credential-request-may-be-answered-by-a-rule-the-judge-reads.md),
    [26-10-12-669](26-10-12-669-jevs-confident-refusal-is-final-and-only-what-it-is-unsure-of-goes-on.md),
    [26-10-14-223](26-10-14-223-a-use-that-drops-a-limit-of-its-line-is-wider.md),
    [26-10-14-691](26-10-14-691-the-judge-is-told-who-is-asking-from-the-control-planes-record.md),
    [26-10-16-965](26-10-16-965-the-omp-judge-answers-with-thinking-off-and-configure-proves-it-can.md),
    [26-10-20-782](26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md),
    [26-10-22-689](26-10-22-689-an-approval-rule-is-a-policy-a-requests-policy-must-fall-within.md).
  - [26-10-02-924](26-10-02-924-a-secret-may-carry-routes-and-every-request-it-is-sent-in-must-fall-on-one.md):
    a route's `use` and judged routes; its routes become §4's.
  - [26-09-30-782](26-09-30-782-a-discobox-answers-its-own-discoboxes-requests-within-what-it-may-delegate.md):
    delegation grants and their judging, replaced by §3.
  - [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)
    §5: grants, uses and use IDs.
  - [26-10-14-320](26-10-14-320-a-discobox-acts-on-any-discobox-in-its-project-and-the-judge-decides-which.md),
    wholly: a discobox acts again only on the discoboxes it created (§9).
  - [0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
    §1 and §5, and §4 in part: the API is a sandbox flag rather than a
    well-known credential (§9), and grants at create become bindings.
  - [26-10-01-240](26-10-01-240-a-swapped-request-records-the-secrets-it-spent.md)
    in part: use IDs become binding IDs.
  - [26-09-25-122](26-09-25-122-a-token-may-expire-and-suggest-the-command-that-renews-it.md)
    §2 and the well-known `RefreshTTL`: a secret's own TTL and refresh command
    are what remain (§9).
  - [0149](0149-a-host-certificate-is-trusted-for-one-sandbox-when-a-person-pins-it.md)
    §5: a pinned host's requests are no longer judged (§10).
  - [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md)
    §3 in part: the harness-config grant a saved credential creates is a
    harness-config binding (§1), as configure's is.
  - The judge sections of the harness ADRs, since no harness provides
    `discobox-prompt`:
    [0127](0127-the-opencode-harness-runs-opencode-1.md) §4,
    [26-10-02-840](26-10-02-840-the-copilot-harness-takes-its-github-token-from-the-environment.md) §5,
    [26-10-14-718](26-10-14-718-the-omp-harness-imports-its-credentials-and-publishes-through-an-extension.md) §5,
    [26-10-15-259](26-10-15-259-the-pi-harness-delivers-auth-json-and-exchanges-copilot-in-the-sandbox.md) §5,
    [26-10-15-848](26-10-15-848-the-opencode-harness-runs-opencode-2.md) §6.
  - [26-10-02-393](26-10-02-393-a-credential-request-and-its-grant-may-name-several-hosts.md),
    implemented without being marked accepted: a request's and a grant's
    host lists stay until bindings carry routes (§1).
  - Proposals this makes moot:
    [26-09-30-854](26-09-30-854-a-request-is-judged-against-the-command-its-sentinel-was-minted-for.md),
    [26-10-25-201](26-10-25-201-source-references-record-upstream-and-fork-for-policy-bindings.md).

## Context

Authorization for a credential is spread across tables that each answer part
of the same question:

| Table | What it holds |
| --- | --- |
| `secrets` | The credential, one `host`, and `routes` (`{host, path, use}`); a route with a `use` is judged |
| `harness_config_secret_bindings`, `sandbox_secrets` | An env var filled by a secret, which is explicitly "not a grant" |
| `secret_grants` | The authorization: scope (sandbox, harness config, project), hosts, uses, policy, purpose (use or delegate), rule and delegation provenance |
| `secret_requests` | Three species in one row: reactive (the proxy met an ungranted sentinel), protocol (an agent asked, with uses and a policy), and refresh |
| `approval_rules` | A policy a request's policy must fall within, with conditions a judge reads |
| `credential_verdicts` | Every judge and Jev decision: prompts, probabilities, standing allows |

A binding and a grant name the same secret and the same target, and are
written together nearly everywhere: the configure flow writes both and records
that "the user just handed this credential to this harness's configure flow,
which is the consent the grant records". The only use of the gap between them
is "delivered, not yet approved", which is what a request is. Delegation is a
third variant of the same authorization, with its own purpose flag that every
grant reader must remember to check.

The judge, Jev and the policy language exist to decide what static checks
could not express. Their decisions are not reproducible, which is why every
one has to be recorded in full; they add a harness or a model to the path of
every credential-bearing request; and what they enforce cannot be read off the
records by a person. We are removing them. What remains has to be expressible
as static checks, and the records have to say plainly who may send what where.

## Decision

### 1. Three records: secrets, bindings, requests

Access to a secret is three concepts, and only three:

- **A secret** is the credential and the furthest it may ever be sent.
- **A binding** is the authorization: a target may do something with a secret,
  within routes, until a time. A binding that exists and has not expired is
  the whole of the permission; there is no grant beside it.
- **A request** is a sandbox asking for a binding, and the decision on it.

```go
type Secret struct {
    ID, ProjectID  string
    Name           string     // unique in the project
    Type           string     // token | oauth | exchange
    EncryptedValue []byte
    Format         string     // sentinel shape
    TTL            int64      // value staleness and refresh, as today
    RefreshCommand []string
    ExchangeRecipe *ExchangeRecipe
    MaxBindingTTL  int64      // was MaxGrantTTL; 0 is no limit
    Routes         []Route    // §4; none means it is sent nowhere
}

type SecretBinding struct {
    ID, ProjectID string
    SecretID      string
    TargetKind    string      // project | harness_config | sandbox
    TargetID      string
    Use           bool        // the target may send it
    Delegate      bool        // the target may hand Use on (§3)
    EnvName       string      // required when Use, absent otherwise
    Routes        []Route     // narrows the secret's; none inherits them
    ExpiresAt     *time.Time  // within MaxBindingTTL
    ParentID      string      // the Delegate binding it was handed on under
    RequestID     string      // the request it answered, if any
    CreatedBy     string      // a person or a sandbox principal
}

type SecretRequest struct {
    ID, ProjectID string
    SandboxID     string      // the requester, and the binding's target
    SecretID      string      // or Name, for one not yet made
    Name          string
    Use, Delegate bool
    EnvName       string
    Routes        []Route
    TTL           int64
    Reason        string      // shown to a person; nothing evaluates it
    Status        string      // pending | approved | denied | expired
    DecidedBy     string
    DecidedAt     *time.Time
    BindingID     string      // what approval created
}
```

`Secret.Host` goes: its matching duty passes to routes and its uniqueness
duty to the name. A request's decision is columns on the request, fixed once
decided, rather than a separate approval record. The approver may edit what is
granted, so the binding, not the request, is what was approved.

`sandbox_secrets` stays as runtime state: one sentinel per binding with `Use`
that applies to the sandbox, keyed by binding rather than secret. It is
derived and never edited by a person. The reactive request goes away: a
sentinel exists only because a binding does, so a request outside the
binding's routes is refused and audited, and nobody is asked. A value refresh
is not a request for access; it moves to its own record.

### 2. A binding applies at three levels, and the narrowest wins

A sandbox is covered by its own bindings, its harness config's, and its
project's. Per env name, the narrowest binding applies (sandbox, then harness
config, then project). Bindings do not stack. A project binding with `Use` is
new: today a project-scoped grant authorizes only bindings made elsewhere.

`Use` and `Delegate` are independent. A binding may carry either or both,
under one set of routes. Different limits for using and handing on are two
bindings. A `Delegate`-only binding has no env name and gets no sentinel.

### 3. Delegation is a binding with `Delegate`, and only a person grants it

A sandbox holding a live `Delegate` binding for a secret, directly or through
its harness config or project, may give `Use` bindings for that secret to the
sandboxes it created (`Sandbox.CreatedBySandboxID`). It does so by approving
their requests or by binding at create (§5). What it gives:

- has `Use` alone, never `Delegate`;
- targets the requesting or created sandbox, never a harness config or
  project;
- records the `Delegate` binding as `ParentID`.

A request that asks for `Delegate` is answered by a person only, and only a
person creates a binding with `Delegate`. No chain grows from a sandbox.

A binding is enforced along its parent: its routes are intersected with its
parent's and the secret's (§4), and it expires by the earliest of its own and
its parent's expiry. So whether a child's routes are "within" the parent's
never has to be decided at approval, which is what the judge was asked when
routes could not be compared. Approval may still refuse an obviously wider ask,
for the approver's sake, but that check is not the boundary. Deleting a
binding deletes the bindings whose parent it is.

### 4. Routes are static and protocol-aware, and a route may be blocked

```go
type Route struct {
    Hosts   []string  // exact, or *.suffix
    Methods []string  // none is any
    Paths   []string  // segment globs: * is one segment, ** the rest; none is any
    GraphQL *struct{ Operations, Fields []string }  // query|mutation|subscription; root fields
    MCP     *struct{ Tools []string }
    Blocked bool      // refuses what it is the most specific route for
}
```

A route list — the secret's, a binding's, each parent's — is read on its own:
the most specific routes a request falls on decide (the longest host, then the
longest path), and a blocked one among them refuses. A blocked route is a
carve-out inside a wider route of the same list, such as Copilot's `/mcp`
under the rest of `githubcopilot.com`; on its own it allows nothing. A request
carrying a sentinel is allowed when every list allows it: the secret's, the
binding's (when the binding has any), and each parent's. No other check runs.
Lists never take precedence over one another, so a binding cannot unblock what
its secret blocks, and a person reading any one list reads all it allows.

Protocol matching reads what the request does, never what it says it does:

- **GraphQL** matches the parsed document's operation type and root fields,
  with root fragment spreads expanded. `operationName` is never read. Batched
  bodies match only if every operation does. A persisted-query hash with no
  document does not match.
- **MCP** constrains `tools/call` by `params.name`. The protocol's own methods
  (`initialize`, `tools/list`, `ping`, `notifications/*`) are allowed under a
  route that names tools. JSON-RPC batches match only if every call does.
- A route with a protocol block needs the body. A body past the capture limit,
  one that does not parse, or one in an encoding the proxy cannot decode,
  does not match. Upgraded connections do not match a protocol block.

Patterns stay that small on purpose: literal values with `*.` host prefixes,
segment globs, and at most a trailing `*` in a tool or field name.

The server computes a sentinel's effective routes (secret, binding, parents)
and pushes them to the pool with the sentinel set, where `Authorize` checks
them per request in place of the judge. Resolution still refuses a value for a
host outside them, so a compromised pool proxy cannot widen a binding.

### 5. Bindings are given at create

Creating a sandbox may carry bindings for it, written in the create's
transaction. A person may give anything. A sandbox creating a sandbox may give
`Use` alone, under a `Delegate` binding it holds, as in §3. Harness config and
project bindings are written through their own resources. A binding written
directly is its own consent: the API's authorization on who may write it
applies, and no request row is made.

### 6. Audit is binding history plus the pool's HTTP audit

A static decision can be reproduced from the request and the routes in force,
so no decision record is kept. Two records stand in for `credential_verdicts`:

- **`secret_binding_events`**, append-only, written in the same transaction
  as every binding change: created, updated, deleted, or expired. Each event
  records the actor (person, sandbox, or the system for cascades and expiry),
  the cause (request or parent), and a snapshot of the binding. Bindings are
  hard-deleted, so without this, whether a sandbox could send a credential
  somewhere last week, and who allowed it, is lost.
- **The pool's HTTP audit** records the binding IDs a request spent, in place
  of use IDs, beside the secret IDs it records today. A refused request names
  the route check that refused it.

`secret_rejections` and the refresh trail are unchanged in purpose.

### 7. What is removed

`credential_verdicts`, `approval_rules`, `secret_grants` and
`harness_config_secret_bindings` are dropped after migration. `pkg/judge`,
`pkg/judge/jev`, `pkg/policy` and `pkg/wellknown` are removed, and with them the pool's judge
routes, the sandbox's judge and proposal routes, standing allows, approved
uses and use IDs.

Approval rules go entirely: the rule resource and its API, rule proposals
from a grant or a request, the rule tests shown on a pending request, rule
verdicts, and a grant's `RuleID`. Nothing answers a request automatically; a
request is answered by a person or, under §3, by the requester's creator.

### 8. Existing data migrates without widening

The database is durable, so migration converts rather than recreates:

- A secret's routes become routes, each host `h` becoming `[h, *.h]` to keep
  today's coverage. `Secret.Host`, when set and no route covers it, becomes a
  route.
- A harness config or sandbox binding with a live grant covering it becomes a
  `SecretBinding` with `Use`, routes from the grant's hosts, and the grant's
  expiry. A project-scoped grant authorizes the bindings it covers this way.
- A binding no live grant covers becomes a pending request.
- A delegation grant becomes a `Delegate` binding on its scope. A grant
  handed on under one gets that binding as `ParentID`.
- **A judged route, or a grant carrying uses or a policy, never becomes an
  unreviewed allow.** Dropping its judge would widen it.
  - A judged route becomes a blocked route on the list it is on (§4): it
    refuses what it judged, and a person who wants it open writes a route
    that says what to allow.
  - A grant carrying uses or a policy that names one sandbox becomes a
    pending request from that sandbox, for a person to approve. A grant a
    rule minted carries a policy, so it is one of these. One wider than a
    sandbox has nobody to ask on its behalf, and is deleted.
  - A host trust approved with uses was judged on every request to its host;
    it is deleted, and its sandbox asks for the pin again (§10).
  - A pool reads a route it stored judged as blocked, until the control plane
    pushes the routes again.
- A pending request a rule would have answered stays pending for a person.
- Verdicts and approval rules are deleted, not converted. A rule's hosts and
  repositories are not a binding: nothing in it names a target to bind.

### 9. Well-known IDs go, and calling the discobox API is a sandbox flag

The well-known registry (`pkg/wellknown`) and every `WellKnownID` go. A request
names a secret by ID, or by name for one a person has still to make; its env
name and routes are what the requester says, and the approver edits them. A
secret's staleness is its own `TTL` and refresh command, with no registry
default.

The registry's one load-bearing entry, `ai.discobox.sandbox`, was the gate by
which a sandbox calls the discobox API through its pool (ADR 0140): a secret
with no value whose live use the pool required. It becomes a field on the
sandbox, `DiscoboxAPI`:

- A person sets it at create, or on an existing sandbox. A sandbox creating a
  sandbox may set it only when it has it itself.
- The pool forwards every call to the gate host, as the sandbox's, and the
  control plane refuses a sandbox that does not have it, before the sandbox
  role is read. The flag lives in one place, read on every call, so the pool
  is sent nothing when it changes. What a call may do is the sandbox role
  (ADR 0140 §4) with the routes ADR 26-10-14-320 §3 added, as today.
- It reaches only the discoboxes the sandbox created
  (`Sandbox.CreatedBySandboxID`): start, stop, tags, terminals and the rest
  of the role act on those alone, as before ADR 26-10-14-320 let a judge
  decide which. Listing still shows the project.
- The upgrade sets it on no sandbox. A grant of the old gate carried a policy
  that no longer applies, so a person sets the flag again where it is wanted.
- It confers the credentials of the project's harness configs: a sandbox that
  has it may create discoboxes on any of them, and each runs with its harness
  config's bindings, as any discobox on it does. That is what a person grants
  by setting it. Beyond that it confers nothing over credentials: what a
  sandbox may give its children is its `Delegate` bindings (§3).

It is not a binding: it names no secret, carries no value, and nothing is
swapped. A change to it is recorded the way any other change to a sandbox is.

### 10. A host trust pins a certificate for one endpoint, and nothing more

A host trust (ADR 0149) stays as it is scoped: one sandbox, one exact
`host:port`, one pinned certificate. It makes that certificate acceptable for
that endpoint and no other, and decides nothing about the requests sent
there. Its uses go, and with them the judging of every request to a pinned
host. A credential sent to a pinned host is held to its binding's routes like
any other. A trust request carries a reason a person reads, as a secret request
does. A trust approved with uses is deleted on upgrade (§8).

## Alternatives rejected

- **Keep grants beside bindings.** The two duplicate the secret and target and
  are written together nearly everywhere. Their only distinct state,
  "delivered but not approved", is a request.
- **A separate delegation table.** A delegation needs the same target levels,
  routes, expiry and revocation as a binding. Kept apart, it is a parallel
  hierarchy whose readers must not be confused with a binding's. Two flags on
  one record cost a branch at materialization.
- **A separate approval record.** It earns its keep with several approvers or
  a history of decisions on one request; neither is wanted. The decision fits
  on the request, and the binding records what was granted.
- **Check a child's routes within its parent at approval.** That needs
  containment over globs and protocol lists, which the judge settled when code
  could not. Intersecting along the parent at enforcement needs no comparison.
- **Static approval rules (auto-approval).** Without a judge, a rule that
  approves every matching request is a standing binding at the project or
  harness config level, which §2 already provides. What that loses is a
  rule's `repositories`: approving only discoboxes whose primary source is a
  given repository. No binding level is a repository. If that is wanted
  back, it is a binding target, not a rule.
- **Match GraphQL by `operationName`.** The client chooses it.
- **The API gate as a secret type, bound like any secret.** It would reuse
  bindings, routes and delegation, but a gate holds no value, takes no
  sentinel swap, and its routes would duplicate the sandbox role. A flag on
  the sandbox says what it is.
- **Keep well-known IDs for their defaults.** They saved an agent spelling a
  host and an env name. With routes and env names on the request, an
  approver who edits them, and the gate a flag, nothing else depends on them.
- **Deny routes across lists.** A route that refused across lists — a
  binding's overriding its secret's — brings precedence a person has to
  resolve. A blocked route refuses only within its own list (§4).
- **A judged route as a pending request** (as first written here). A route is
  a secret's or a harness config's, not a sandbox's: there is nobody to file
  it, and a secret whose judged route was taken away falls back to a wider
  route of its list, which would widen it. Blocked keeps the carve-out shut.
- **The pool checks the API flag.** The flag would have to be pushed to every
  pool and kept in step with the control plane, which reads its own record on
  every call anyway.
- **Routes on host trusts.** A trust is about which certificate to believe;
  what may be sent is a binding's question, and a request carrying no
  credential spends nothing a route would protect.

## Consequences

Who may send a credential where is read from bindings and routes alone, and
the same request is always decided the same way. No harness, model, or
control-plane round trip sits in the path of a credential-bearing request
once its value is cached. Authorization outside what routes can express (a
repository's identity in a REST path aside) is no longer available. A person
narrows with routes or does not bind.

The API, CLI and console lose grants, approval rules, verdicts and rule
proposals, and gain bindings at all three levels. Harness configure writes one
binding where it wrote a binding and a grant.

## Deferred

- **How an agent in a sandbox asks for a credential and receives it**: the
  discobox-access protocol's redesign, including whether a binding is ever
  activated per command rather than injected (ADR 0031 §4's ephemeral
  sentinels). Until a later ADR decides it, discobox-access is a minimal
  client of these records: it lists what the sandbox may use, files a
  request, and waits on it. It gains one verb meanwhile, `environment`
  (`discobox-access env`): the credential variables and the sentinels in them
  now. A harness's tool shell inherits the environment the harness was
  launched with, so without it a credential granted mid-session would reach
  only processes started afterwards; restarting the harness was the
  alternative, and loses the session. A sentinel only resolves from its own
  sandbox, so printing it gives nothing away. A discobox answers a request of one it created
  under §3 through the discobox API, as it does today. An approved binding is
  delivered as an injected sentinel; per-command `run`, activations and
  `AgentRequested` go.
- **Retention of the pool's HTTP audit** stays at the pool's window. If
  credential use must be kept longer, or outlive its pool, forward the audit
  off the pool rather than adding a control-plane decision record.
