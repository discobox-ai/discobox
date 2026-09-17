# 0130 — An audit record is read where it was written, and names its attestor

- **Status**: Proposed
- **Date**: 2026-09-16
- **Relates to**: [ADR 0091](0091-a-credential-is-not-issued-without-a-verdict-on-record.md),
  whose closing paragraph — "nothing in the sandbox is a place to keep a record
  about the sandbox" — is the trust boundary this draws a line along, and whose
  verdict trail this makes readable.
  [ADR 0081](0081-project-events-are-not-persisted-and-the-wait-polls.md) dropped
  the one table that looked like a general audit log; this does not bring it
  back.
  [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md) §3
  owns the ephemeral sentinel that §3 here declines to store.
  [ADR 0112](0112-the-top-level-is-for-people-not-a-transport-diagnosis.md)
  decides where §5's commands live.

## Context

Four sandbox-scoped trails exist, in three databases, and one of them is
reachable from the CLI.

**The proxy trail** is the richest and the least reachable.
`proxy/internal/audit` writes `http_exchanges` and `socks_connects` to a
pool-local SQLite database at `layout.ProxyAuditDB(project, pool)`, keyed by the
mTLS client ID, which is the sandbox ID. A row carries method, URL, host,
status, duration, block decision and reason, cache state, the applied rewrite
rule, redacted headers, byte counts, and the names of body and upgraded-stream
spool files on the same disk. Retention is 48h and deliberately survives sandbox
deletion. The read side is written and tested — `ControlHandler` serves
`GET /audit/http`, `/audit/socks`, `/audit/dropped`, and
`/audit/http/{id}/{request-body|response-body|stream}` behind a PASETO v4.public
token for audience `discobox-proxy-control` with scope `audit:read`, refusing a
token whose `sandbox_id` does not match the `client_id` being read. None of it
runs: `pool-agent/proxyagent` sets no `Control` config, and
`proxy.CreateControlToken` has no caller outside tests.

**The verdict trail** is kept on trusted ground and unreadable.
`credential_verdicts` (ADR 0091) lives in the control-plane database, outlives
its sandbox, and holds the argv, the judge's reason, the full prompt, the role
and the latency for every credential this control plane ever issued.
`Store.ListCredentialVerdicts` exists and says of itself: "Nothing in this
repository serves it over HTTP yet."

**The harness hook trail** is reachable and is not evidence.
`harness_hook_logs` in the sandbox-agent database collects `SessionStart`,
`PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Notification`,
`SubagentStop` and `Stop` with their raw provider payloads, which is a per-tool-call
record of what the agent did. `discobox admin hooks logs` reads it through the
control plane's reverse proxy, filtered by terminal and limit.

**The exec trail** is half-reachable. `exec_events` (`exec.created`,
`exec.started`, `exec.stop.requested`, `exec.start.failed`, `exec.attach.opened`
and the rest) and `exec_log_chunks` (compressed transcripts, 14-day retention,
ADR 0028) sit beside the hooks. `list-sandbox-exec-logs` has a CLI command;
`list-sandbox-exec-events` has an API operation and no CLI command at all.

Three facts shape what can be built on these.

**Two of the four are editable by their own subject.** ADR 0091 established it
and the reasoning has not changed: the sandbox-agent database is `root:root
0644`, the agent has sudo, and the hook socket is `0666`, so any process in the
sandbox can forge a hook row and root in the sandbox can rewrite any of them.
Both trails also die with their sandbox. The proxy and verdict trails are kept
on the far side of a boundary the sandbox cannot cross, so no row can be altered
once written, and both outlive it — though a verdict is only as good as the
sandbox's account of it, which §2 draws out.

**No single database can hold all four.** The proxy trail is pool-local by
construction — the spool files it names are on the pool's disk, and the volume
is every HTTP request every sandbox makes. The hook and exec trails are
sandbox-local and already proxied on demand. Only the verdict trail is
control-plane state, and it is small precisely because it is one row per
credential use.

**The two trusted trails do not meet.** The proxy knows a sentinel was swapped
into a request — `swapSecrets` traces it as a span — and records nothing about
it beyond adding the affected header names to the redaction set. So "every
outbound request that spent grant X" is unanswerable, which is the question the
verdict trail exists to make askable and the proxy trail holds the other half
of.

## Decision

**Audit records are read from where they were written. The control plane fans
out, merges, and labels; it copies nothing. Every row names the party that
attested it, and a swapped request records the use it spent.**

### 1. Each source stays where it is, and the server fans out

`discobox admin audit` is one CLI surface over four readers, not one table. The
control plane queries the proxy control API on the pool, the sandbox-agent
through the reverse proxy it already runs, and its own database directly, then
merges by timestamp.

The merge is the control plane's because it is the only party that can see all
four and the only one that knows which sandboxes a caller may read. Nothing is
copied forward: a copy would need its own retention, its own migration and its
own answer to what happens when the copy and the original disagree, and for the
proxy trail it would mean replicating every HTTP request every sandbox makes
into the control-plane database.

Fan-out has a real cost and it is accepted: a pool that is down or a sandbox
that is deleted makes part of the answer unavailable. The CLI reports the
unreachable source by name rather than returning a short answer that looks
complete. A trail that silently omits what it could not reach is worse than one
that says so.

### 2. Every row carries its attestor, and the CLI never mixes them silently

Each record is labeled with who vouches for it:

- `control-plane` — credential verdicts recorded at use. Written on trusted
  ground on the call that mints a value; the sandbox cannot reach the row once
  it is written. What that attests is the issue — that a value was taken with
  this verdict attached — and not the verdict's content: the judge runs inside
  the sandbox (ADR 0079), so the argv, reason and prompt are the sandbox's
  account whichever way the row arrived.
- `sandbox` — credential verdicts recorded by report. A denial never reaches
  the use call, so its row exists only because the sandbox chose to send it
  (ADR 0091 §3). The control plane keeps it, and nothing vouches for it.

  For a verdict the attestor is derived rather than stored: `volunteered` false
  is `control-plane`, true is `sandbox`. `admin audit creds` reads one trail and
  shows the same split under that trail's own names, `use` and `report`; the
  merged `list` is where the attestor itself is the column.
- `pool` — proxy HTTP and SOCKS rows. Written by the pool proxy from what
  crossed the wire; the sandbox cannot reach the row.
- `sandbox` — harness hooks and exec events. Written inside the sandbox, by a
  process the sandbox controls, into a database the sandbox can rewrite.

The distinction is a field on the record and a column in the default output, not
a footnote in the documentation. A `sandbox`-attested row is a diagnostic: it
says what a cooperating agent reported, which is useful and is not evidence. Put
the three side by side with no marking and the interface launders the third into
the other two.

`--attestor` filters on it, and `--trusted` is the shorthand for excluding
`sandbox`.

### 3. A swapped request records the use it spent, by ID and never by sentinel

`proxy/internal/secrets.ResolveResult` gains a `UseID`, the `Swapper` carries it
from the resolver through its cache onto `Result`, and the audit row gains
`SwappedUseIDs`. `pool-agent/proxyagent`'s resolver fills it from the activation
it already looked up to translate the ephemeral sentinel — `secretResolver.activation`
returns the record with `UseID` on it, and today the field is read for its
`Stable` and `ExpiresAt` and discarded.

This is the join. With it, one use ID reaches the verdict that authorized a
command and every request that actually spent the credential, from two trails
that no operator could previously line up by hand — the proxy row's timestamp
and host are not enough to distinguish two uses of the same credential minutes
apart.

**The ID, never the sentinel.** An ephemeral sentinel is a bearer token for five
minutes (ADR 0031 §3's use clock): anything holding it can take the real
credential from the proxy until it lapses. Writing it into a database that keeps
rows for 48h to be read later puts a live credential into the artifact whose
whole purpose is to be read after the fact. The stable sentinel is worse — it
does not expire, and ADR 0031 withholds it from the sandbox for exactly that
reason. The use ID authorizes nothing; it only names.

The field is plural because one request can swap more than one sentinel: Git
sends a username and a password in one `Authorization: Basic` token, and
`swapEncoded` resolves each half independently.

A swap from an ordinary injected sentinel — the reactive path, with no agent
credentials protocol behind it — has no use to name and leaves the field empty.
The row still records that a swap happened, through the redaction it forces. The
asymmetry is the truth: only a credential taken through the protocol has an
approved use at all.

### 4. The pool trail is read through the control API that already exists

The control plane never talks to the proxy. It asks the pool agent, over the
pool-agent API it already signs requests to, and the pool agent relays to the
proxy's control API with a token it mints itself. No new protocol and no new
listener design: the authorization model was built for this, and the pool-agent
route is the pattern every other pool-local read already uses.

- **The proxy key never leaves the pool.** `pool-agent/proxyagent` generates the
  control keypair with the rest of the pool's proxy material, beside the MITM
  CA key under `layout.ProxyCerts`, which already has exactly that custody. The
  proxy unit is configured with its public half as `Control.TrustPublicKey`;
  only the pool agent reads the private half, per request, to sign a
  `CreateControlToken` that lives five minutes. The agent prepares the key at
  startup, before systemd starts the proxy, and is the only writer: a key that is
  present but unusable is replaced there, the way an unloadable CA is. A proxy
  that cannot read the key serves sandbox traffic with no control API, rather
  than not at all — an audit read must not be able to cost the pool its egress.
- **The listener is loopback.** `Control.ListenAddress` binds `127.0.0.1` in
  the pool, never `0.0.0.0` as the proxy's own port does. The pool agent shares
  the pool's network namespace with the proxy unit. A sandbox does not: sandboxes
  are sibling containers on the pool's internal network, each in its own network
  namespace, reaching the pool container by a network alias, so the pool's
  loopback is not an address any of them has.
- **Scope carries through.** The control plane's pool-agent token is scoped
  `audit:read` and names the sandbox when the read does; the pool agent copies
  that sandbox into the proxy token's `sandbox_id`, so the narrowing below
  applies to it. A read that names no sandbox is the pool-wide read, and the
  control plane only asks for one on behalf of a project member.
- **The control plane reads the project, and fans out over its pools** (§1).
  `/projects/{projectId}/audit/http` asks the pool `poolId` names; otherwise,
  when `sandboxId` names a sandbox that still exists, the pool that sandbox runs
  on, since a sandbox's pool is fixed for its life; otherwise every pool in the
  project. It merges newest first. It is not a route under the sandbox: a
  purged sandbox's rows stay on its pool for the retention window, and the row
  that said which pool that was is gone with the sandbox.
- **A pool that cannot answer is reported, and does not hold the answer.** Each
  pool is read under its own deadline, and a pool being deleted or whose agent
  never registered is reported without being asked. Reaching an agent is only
  attempted, never recovered: the path that reconciles an unreachable pool and
  waits for it exists for operations that need the pool running (ADR 0039), and
  a read must not restart pools. Whatever the reason — no agent, a timeout, an
  agent that predates the operation — the pool is named in the response beside
  the rows that did arrive, never dropped from it.

Both settings, not either. `newControlAuthenticator` returns a nil
authenticator for an empty trust key and `Middleware` then passes every request
through, so an address configured without a key is an unauthenticated audit API
rather than a closed one. Setting one without the other is a misconfiguration
the pool must not be able to express.

That model did not hold when this was written, and how it failed is worth
recording. `controlAuthenticator.authorize` compared a token's `sandbox_id`
against the request's `client_id` only when the request sent one, so a
sandbox-scoped token that simply omitted `client_id` read every sandbox's rows —
and, through `handleControlHTTPArtifact`, their spooled request and response
bodies. It survived because nothing serves the control API, which is the same
reason §3's `use_id` would have shipped on top of it.

The fix is that a sandbox-scoped token **narrows** the read rather than being
compared against it, and that it narrows in the middleware rather than in each
handler. Per-handler narrowing would have been correct on the day and wrong on
the day after: the hole was a check a route could decline to trigger, and
repairing it with a call every future route must remember to make reproduces the
shape of the bug. Rewriting `client_id` once, where every route already passes,
leaves no request shape that can express the unnarrowed read.

### 5. What the CLI surfaces

The commands live under `admin`, beside `admin hooks` and the exec log reads
they sit with. [ADR 0112](0112-the-top-level-is-for-people-not-a-transport-diagnosis.md)
keeps the top level for the things somebody does with a discobox and puts what
exists to inspect or operate the system under `admin`; reading a trail is the
second kind.

```
discobox admin audit list  [--since] [--source] [--attestor] [--trusted] [-f]
discobox admin audit http  [--discobox-id] [--pool] [--host] [--use-id] [--since] [--limit]
discobox admin audit creds [--discobox-id] [--use-id] [--grant-id] [--denied|--allowed] [--since] [--prompt]
discobox admin audit hooks [--provider] [--event]
```

`http` does not yet take `--status`, `--blocked`, `--body ID` (a recorded
request or response body, or an upgraded stream) or `--follow`; `list` and
`hooks` are not built. Those are what remains of this section.

`creds` reads the project, not a sandbox: `list-credential-verdicts` is
`/projects/{projectId}/credential-verdicts` with the sandbox as a filter, because
a route under `/sandboxes/{id}` invites a handler that looks the sandbox up
first and answers 404 for exactly the purged sandboxes the trail outlives.

Two mechanical gaps close with it. `audit.QueryOptions` carries `ClientID`,
`Host`, `UseID` and `Limit`, and grows a time bound and an `after_id` cursor,
because a `--follow` that re-fetches the last hundred rows on every tick is not
a tail. `list-harness-hooks` grows the same.

### 6. Everything a sandbox wrote is display data

The argv, the facts block, the judge's reason, and every hook payload are text
composed inside a sandbox. ADR 0090 §3 and ADR 0091's consequences already say
this for the verdict; it holds for the whole surface, and the audit CLI is where
it is most likely to be forgotten, because its output is the thing most likely
to be piped into another agent. Rendered as data, never as instruction, by the
control plane and by anything reading the trail downstream.

## Alternatives rejected

**Put `audit` at the top level.** The first draft of §5 did, arguing that
reading what an agent did with your credentials is about your own work, the way
`secret` is. Rejected under ADR 0112: the top level is for doing work with a
discobox, and a trail is read to inspect what happened, which is `admin`'s. The
commands are reached from documentation and from the error hints that name them,
not by browsing, so the placement costs little discoverability.

**Copy all four trails into one control-plane audit table.** The obvious shape,
and the one that makes `--follow` and the merge trivial. Rejected on the proxy
trail alone: it is every HTTP request every sandbox makes, it names spool files
that exist only on the pool's disk, and copying it would mean either shipping
those files too or keeping rows whose artifacts cannot be fetched. The hook and
exec trails add the second problem — a copy has to win a race against sandbox
deletion for every sandbox forever, which is the same reasoning ADR 0091 used to
reject pulling verdicts out of the sandbox.

**Revive `project_events` as the audit log.** ADR 0081 dropped it, and its
reasoning holds: a full JSON serialization of every mutated resource, written
unconditionally, indexed six ways and read by nothing. Sandbox lifecycle history
is worth having and is a different thing from this — it is an intent trail, not
an observation trail, and it should be designed for the question it answers
rather than resurrected because a table used to exist. Out of scope here.

**Store the ephemeral sentinel on the audit row instead of the use ID.** It is
what the proxy has in hand without touching the resolver contract, and it joins
to the activation directly. Rejected: see §3. It is a live bearer token and the
audit database is the wrong place for one.

**Record the join on the control plane instead, by having the proxy report each
swap.** Keeps the pool trail unchanged and puts the join where the verdict
already is. Rejected: it is a second write path from the pool to the control
plane on the hot request path, for information the proxy is already writing a
row about. The row it is already writing is the right place.

**Have the control plane mint proxy control tokens.** What the first draft of
§4 said, and it matches the proxy's existing design, where
`CreateControlToken`'s caller holds the private key. Rejected: it gives the
control plane a second long-lived key whose only use is a request it already has
an authenticated channel to the pool for, and it needs the control listener
reachable from off the pool — the one listener this ADR wants on loopback. The
pool agent is already the proxy's trusted neighbor: it prepares its
certificates and holds its CA key.

**Let the CLI talk to each source directly.** No fan-out in the server, no merge
to maintain. Rejected: it would put the pool's address, the sandbox's address
and a proxy control token in the client, which moves the authorization decision
about which sandboxes a caller may read out of the control plane and into
whatever the client chooses to send.

**Keep a project's verdicts past the project.** Drop the foreign key and leave
the rows. Rejected: every read of the trail is scoped by project, so rows whose
project is gone are reachable by nothing and keep only their cost, and removing
the constraint is a table rebuild on SQLite for that.

**Present one merged stream with no attestor column.** Simpler output, and the
records genuinely are about the same sandbox. Rejected: see §2. This is the
whole reason the sandbox-side records are cheap to collect, and hiding it is how
a forged hook row ends up quoted as evidence.

**Fix the custody problem first, then build the CLI.** ADR 0091's closing note
calls for sandbox-side records to move to append-only remote storage, which
would collapse §2's three attestors into one. Rejected as a precondition: it is
a larger piece of work than this, the proxy trail and the verdicts recorded at
use are readable without it, and labeling the rest honestly is what makes
shipping before it safe. It stays deferred, and §2's attestor field is what a
later change would flip from `sandbox` per source as custody moves. A reported
denial stays `sandbox` either way: its row is already in custody, and what it
lacks is a witness.

## Consequences

- `proxy/internal/secrets.ResolveResult` grows a field, so every `Resolver`
  implementation compiles unchanged and only `pool-agent/proxyagent` fills it. A
  resolver that does not is not wrong; it is one with no uses to name.
- `http_exchanges` gains a column, deliberately unindexed: the only query that
  reads it matches one element of a comma-joined list, which is an expression
  with a leading wildcard and cannot use a B-tree index, so an index would be
  write cost on the schema's highest-volume table and nothing else. The scan is
  bounded in practice by `client_id`, which is indexed and which every real
  caller sends. AutoMigrate adds the column; rows written before it carry an
  empty value, which reads as "no use recorded" rather than "no credential
  spent" — for a row predating the column those are indistinguishable, and the
  window is bounded by the 48h retention.
- Pool proxies start serving a control API on a listener that did not exist
  before. It is authenticated, read-only, narrows every sandbox-scoped read, and
  binds loopback, so it is new surface only to what already runs inside the
  pool.
- An agent that predates the audit operation answers the relay with its router's
  404. The control plane reports that pool as unavailable, by name, rather than
  as an empty trail.
- A `--follow` over four sources is four polls at four cadences. The proxy trail
  is the high-volume one and the others are quiet; nothing here makes any of
  them a push.
- The verdict trail becomes readable by whoever can read the project, which is
  the point and is also the first time a judge's full prompt leaves the
  database. It contains the facts block and the argv, both composed inside the
  sandbox, and neither contains a credential value — ADR 0091 §4 makes that
  structural rather than a matter of filtering.
- `discobox admin hooks logs` moves to `discobox admin audit hooks`. It is an
  admin debug command with no compatibility promise.
- Deleting a project deletes its verdicts. They reference the project, and until
  they were part of its delete a project with a single recorded verdict could
  not be deleted at all — ADR 0023 deletes only an empty project, so every
  project that had ever issued an agent credential was stuck. A verdict outlives
  its sandbox, not its project. ADR 0091's objection is to a record its own
  subject can erase, and a project is deleted by its members, not by the sandbox
  a verdict describes.
