# 26-10-02-393 — A credential request and its grant may name several hosts

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md) (implemented without having been marked Accepted)
- **Date**: 2026-10-02
- **Relates to**: [ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)
  §§1, 5, which gave a request and its grant one host;
  [ADR 26-10-01-240](26-10-01-240-a-swapped-request-records-the-secrets-it-spent.md)
  §3, which holds that a pool agent, a CLI, and the server they talk to are
  upgraded separately.

## Context

An agent credentials request names one host, and the grant approving it is
limited to that host and the hosts beneath it (`hostscope.Covers`). Every
check downstream reads that one host: the grant lookup a resolve makes, the
activation the pool agent mints for a use, the use the judge is asked about,
the delegation a discobox approves under.

Some tools send one credential to two sites that share no parent. Copilot CLI
authenticates with one GitHub token and sends it to `api.github.com` and to
`api.githubcopilot.com`. Today an agent has three options and all are wrong:

- Two requests and two grants — but the CLI runs a command under one use,
  and the value the pool mints for it is pinned to that use's grant's host, so
  the process holding it is refused at the other site.
- A wildcard grant, which this flow refuses to mint (ADR 0031 §5) for good
  reason.
- A grant for the common parent, which for these two is `com` and is refused
  as too broad.

## Decision

### 1. A request and its grant carry a list of hosts

`SecretRequest.Hosts` and `SecretGrant.Hosts` replace `Host`. A grant covers a
destination when any of its hosts covers it, and an empty list remains the
wildcard — still minted only by an explicit `secret grant create`, never by
approving an agent's ask. Hosts are normalized the one way `hostscope` does,
deduplicated, and kept in the order they were asked for.

Where one grant has to be picked among several that cover a destination, its
specificity is that of its closest host.

### 2. Every check asks "does any host cover this destination?"

The grant lookup, `ApprovedUse`, the pool agent's activation check, and the
delegation check all read the list. An approved use is reported to the judge
with the grant host that covers the destination it is going to, so the judge
is told where *this* request is approved for, not a list it must match itself.

A grant is checked against its secret host by host: each must sit inside the
secret's binding. A secret binding stays one host. A credential that genuinely
spans unrelated sites is an unbound secret, as the grant-host guard already says,
and the approval can release the binding in the same act (`secretHost`).

A delegated approval's hosts must each be covered by the delegation grant's.

### 3. Two requests ask the same when they name the same set of hosts

The pending-request dedup compares the host lists as sets: approving either
would grant the same thing, so a retry that lists them in another order is a
retry.

### 4. `hosts` replaces `host` on every wire

One field, end to end: the CLI's `--hosts`, the server's request, grant,
approve, grant-create, and sandbox-grant shapes, the pool agent's calls to the
server, and the agent credentials protocol. There is no `host` beside it and no
rule for reading two spellings together.

The agent credentials protocol changes in place, under `v1`. Its only clients
are this repository's own `discobox-access`, and the skew a `/v2` would serve
lasts until a discobox is upgraded or recreated — not long enough to keep a
second shape of every route for, when the old one fails closed (below).

A body that leaves `hosts` out takes the caller's default — the secret's host
for a grant, the request's hosts for an approval, a well-known credential's
first host for an ask by ID. A body that sends an empty list names no host,
which is how a grant asks for the wildcard.

The components are upgraded separately (ADR 26-10-01-240 §3), and the skew
fails closed rather than wide:

- **An older CLI or console** refuses the server's grants and requests, which
  carry a field it does not know, and the server refuses its `host`. It is
  upgraded with the server; the CLI keeps no compatibility with older servers.
- **An older pool agent** sends `host` on an agent's ask, which the server
  refuses, so the agent's request fails until the pool is upgraded. It reads no
  host from `sandbox-credentials` and so skips its local activation check, but
  the control plane still holds every swap to the grant's hosts at resolve time
  and every judged use to them in `ApprovedUse`, so nothing travels further.
  Its gate admits the discobox API only to an activation for exactly the gate
  host, which one with no host is not, so discoboxes on that pool lose the
  `ai.discobox.sandbox` access they were already granted until it is upgraded.
- **An older `discobox-access`** is baked into the sandbox image, so a discobox
  made before the upgrade keeps it, and the skill telling its agent to send
  `host`, until it is upgraded or recreated. It sends `host` on every ask,
  empty when it named none, so an ask by ID alone still goes through. One that
  names a host is refused, telling the agent its sandbox needs upgrading or
  recreating, rather than read with the field dropped: dropped, an ask by ID
  that named a host beneath the ID's would come back as an ask for the ID's
  whole site, wider than the agent meant and what a person would be shown to
  approve. Its `list` shows no host, and its command judge is told an empty
  approved host.

### 5. A well-known credential may list more hosts than it asks for

`com.github.api` gains `githubcopilot.com`. A well-known credential's `Hosts`
already meant "where it may be sent", and an ask by ID that names no host still
asks for the first alone, so nothing that asks for GitHub today is widened to
Copilot. An ask by ID that names hosts has each checked against the list, so
`com.github.api` asked for at `api.github.com` and `api.githubcopilot.com` is
one request, one grant, and one use.

### 6. The column is migrated, not recreated

`secret_requests` and `secret_grants` gain a `hosts` column (a JSON list).
Migration backfills it from `host` — `[host]` where `host` is set, empty where
it is not — and then drops `host` with `dropRetiredColumn`, both guarded by
whether `host` still exists, so the step is idempotent.

Downgrading the server past this is not supported. An older server's
`AutoMigrate` re-adds `host` empty, and empty is the wildcard, so every grant it
reads would be unscoped. Keeping `host` written with the first host would make a
downgrade fail closed instead, at the price of a column the server writes and
never reads; it was weighed and declined.

## Alternatives rejected

- **Two grants and two uses, one per host.** The CLI runs a command under one
  use and puts one value in one variable, and that value is pinned to one
  grant's host, so a single process cannot carry a value valid at both. Making
  one value stand for two uses at once is a larger change to the protocol than
  a list, and the person approving is still asked twice about one credential.
- **Approve at the common parent.** For the case that motivates this, there is
  none short of a public suffix, which `hostscope.TooBroad` refuses.
- **A list on the secret binding too.** It would let a GitHub secret say "this
  is for github.com and githubcopilot.com". Deferred: an unbound secret already
  expresses a credential that spans sites, the approval can release a binding
  in the same write, and the binding is part of the secret's unique index,
  which a list would have to leave. Revisit if releasing bindings to use
  multi-host grants becomes the common path rather than the exception.
- **Keep `host` beside `hosts` on every wire.** It would let an older pool
  agent and in-sandbox CLI keep working at a list's first host. Rejected: two
  spellings of one field, read together by a rule at every boundary, to smooth
  a skew that an upgrade or a recreated discobox ends, and that already fails
  closed (§4).

## Consequences

- An agent asks once for a credential it sends to several sites, and a person
  approves one grant for all of them.
- A GitHub secret bound to `github.com` refuses a grant for
  `githubcopilot.com`; the approval names the remedy, releasing the binding.
- The approval inbox, `secret grant list`, and the console show a grant's
  hosts joined, and `--hosts` on the CLI takes them comma-separated or
  repeated.
- A pool agent or CLI older than the server stops asking for credentials until
  it is upgraded, and a pool agent older than the server also stops admitting
  its discoboxes to the discobox API. An in-sandbox CLI older than the server
  may still ask by a well-known ID alone, but not name a destination — no
  free-form ask, no ask by ID narrowed to a host — until its discobox is
  upgraded or recreated.
