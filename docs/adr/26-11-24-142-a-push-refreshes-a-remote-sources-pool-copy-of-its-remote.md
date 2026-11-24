# 26-11-24-142 — A push refreshes a remote source's pool copy of its remote

- **Status**: Proposed
- **Date**: 2026-11-24
- **Issue**: [#1730](https://github.com/discobox-ai/discobox/issues/1730)
- **Extends**:
  [26-10-23-948](26-10-23-948-a-pool-pulls-a-cached-source-first-and-a-client-supplies-what-the-pull-could-not.md)
  §§1–5 from a source's first delivery to any later refresh of it, and takes
  up on demand its Deferred "refreshing a cache's remote refs" (not on a
  schedule, which stays deferred);
  [26-11-04-253](26-11-04-253-git-credential-delivery-is-an-explicit-client-policy.md)
  §§1–2 to `discobox push`.
- **Relates to**:
  [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md) (`discobox push`),
  [26-10-10-545](26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md)
  §§5–6 (a source's namespace, the box's `discobox` remote),
  [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md).

## Context

Checked against `main` at `e6f5ea446`.

A box created from a private remote can't get commits the remote gained after
the box was created. Issue #1730's user wanted the box to merge a `staging` that
had moved 255 commits, and none of the three ways they tried works:

- **`discobox push` skips the source.** `sandboxpush.CheckPushDelivered`
  (`cli/internal/sandboxpush/push.go`) refuses any source without a
  `localDirectory`: "its source is a remote, which the discobox fetches from
  itself". The command's help (`cli/internal/cli/push.go`) says the same.
  That holds only for a public remote.
- **The box can't fetch the remote.** `origin` in the box is the real remote
  (545 §6), and the box holds no credential for it. As 253 §2 says, "neither
  option grants a box access to the client's upstream credentials".
- **The pool's copy is frozen.** At materialization the sandbox agent fetches
  the namespace's `refs/remotes/origin/*` into the checkout's `origin/*`, once
  (`configureOrigin`, `sandbox-agent/sourceconverge/git.go`). Nothing writes
  those namespace refs after delivery. The client pushes them at create
  (`cacheDelivery.deliverRemote`, `cli/internal/sandboxcreate/cachedeliver.go`),
  or the pool writes them when its own pull pins (`gitcache.Store.Pull`,
  `pool-agent/gitcache/pull.go`). The lent-credential route
  `POST .../sandboxes/{s}/sources/{slug}/pull`
  (`server/internal/resources/sandboxes/source_pull.go`) answers 409 unless the
  source is `needs_credential`, and is a no-op once it is delivered.

The pieces a refresh needs exist already. They are wired for the first delivery
only:

- `gitcache.Store.Pull` fetches over HTTPS into temporary refs. It writes the
  cache's shared remote refs, then the namespace's `refs/remotes/origin/*` and
  `refs/tags/*`, from what this pull fetched and nothing else. A credential
  reaches it only through the process environment.
- `deliverRemote` fetches into the client's own cache of the remote,
  negotiating first against what the pool holds, and pushes
  `+refs/remotes/origin/*` into the namespace.
- 253's `git.credentials` policy (`ask`, `lend`, `local`) decides between those
  two when a credential is needed.

The workaround the user found was a bundle made on the host, copied in with
`discobox cp`, and fetched from a file in the box. It is the `local` path
done by hand.

## Decision

### 1. A refresh updates the namespace's record of the remote, never its branches

A **refresh** of a source whose cache key is a remote (545 §2) brings the
namespace's `refs/remotes/origin/*` and `refs/tags/*` to what the remote has
now. It also removes the namespace's remote-tracking refs for branches the
remote no longer has, the way `git fetch --prune` does. It also updates the
cache's shared remote refs, outside every namespace, as a create's pull does
(948 §1), so the next create from that remote starts closer.

A refresh **never writes the namespace's `refs/heads/*`**. Those are the branches
the box's checkout reaches as `discobox/<branch>`, which a create or a
`discobox push` of a local source set. A refresh **never changes the
source's delivery state**: a delivered source stays delivered at its checkout
commit, and nothing about the sandbox's phase changes. Like 0058's push, it is
transport only.

Inside the box the refreshed refs are picked up when the work there is ready
for them:

```
git fetch discobox '+refs/remotes/origin/*:refs/remotes/origin/*'
```

This leaves the checkout's `origin/*` as a `git fetch origin` with a credential
would have. The default refspec of the box's `discobox` remote is unchanged
(see Alternatives).

A source with no remote has nothing to refresh (948 Deferred).

### 2. `discobox push` refreshes a remote source

`discobox push` stops skipping a source that is a remote. For each such source
it refreshes (§1) instead of sending a local branch:

- With no `--branch`, it refreshes every branch and tag of the remote.
- With `--branch B`, it refreshes only the remote's branch `B`, and the
  remote's tags. Then `push --branch staging`, the spelling the issue reached
  for, does what it says. `B` doesn't exist on the remote: that source fails,
  naming it.
- `--force` has no meaning for a refresh. Nothing is leased, and the
  remote-tracking refs follow the remote as a forced fetch does. It is refused
  when `--source` names only remote sources, and ignored for them otherwise.

When a box has both kinds of source, a push with no `--source` pushes the local
ones and refreshes the remote ones. The command's output and help say which
one each source got, and give §1's in-box command for a refreshed one. The help
no longer says the box "fetches that remote itself".

The console's automatic push (`push_auto.go`) **does not refresh**. It runs
every few seconds without a person there, and a refresh reaches the remote and
may need a credential decision. A refresh is only ever something a person asks
for.

### 3. The pool pulls first, then the client's credential policy decides

A refresh follows 948's order for a first delivery, with 253's policy:

1. **The pool pulls anonymously.** The client calls the source's `pull` route
   with no credential, and the pool refreshes as `gitcache.Store.Pull` pulls
   (948 §§1–2: HTTPS, no helper, no prompt, a stall bound). A public remote is
   refreshed with its bytes crossing once, forge to pool, and no question is
   asked.
2. **The pull is refused for authentication** (`needs_credential`, with the
   protocol, host and path, as 948 §3 records it). The client then does what
   `git.credentials` says, resolved as 253 §1 resolves it. `push` gains
   `--git-credentials=ask|lend|local` with the same precedence: flag, then
   saved setting, then `ask`.

| Policy | Refresh |
| --- | --- |
| `lend` | The client fills a credential as 948 §5 says and calls the `pull` route again with it. The pool refreshes inside that request. |
| `local` | The client fetches into its own cache of the remote with this machine's Git configuration, negotiating first against the pool's copy as `deliverRemote` does. Then it pushes `+refs/remotes/origin/*` and `refs/tags/*` into the namespace (only the branch `--branch` names, when it names one). |
| `ask`, interactive | The same question and explanation as 253 §3, for this source. |
| `ask`, no interactive input | The source is reported **skipped**, with the policy flags and the setting's path, and no credential helper runs. If `--source` names it, the source fails instead. |

3. **The pull can't reach the remote, or stalls** (`unreachable`). The
   client falls back to `local`, as 948 §4 does for a delivery, and its status
   output says so.

253 §2's rules hold unchanged. A sandbox client never lends: explicit or saved
`lend` inside a box is rejected with instructions to use `local`. Its `local`
fetch goes through its own proxy. The server's refusal of a sandbox
principal's credential stays. 253 §4's no-question execution covers the
`local` fetch of a non-interactive push.

### 4. The `pull` route refreshes a delivered source

`POST .../sandboxes/{s}/sources/{slug}/pull` gives up its "only while owed"
rule:

- **Its credential becomes optional.** Without one the pool pulls anonymously.
  With one, the credential still exists only for that request, under every
  rule in 948 §5 and 253 §3.
- **On a delivered source the call is a refresh** (§1). The answer is the
  pull's recorded result (refreshed, `needs_credential`, `unreachable`) and
  the refs it moved. It does not use 401 for "credential refused" on an
  anonymous attempt, because that is an expected answer and not an error.
- **On a source still owed**, the route keeps today's meaning. A pull that holds
  the checkout commit completes the delivery, as now.
- **Who may call it is unchanged**: `sandbox:write` on the box. A sandbox
  caller may refresh anonymously, as 948 §6 already lets it make its pool pull
  any public HTTPS remote into that remote's cache.

On the pool, `gitcache.Store.Pull` gains the refresh form. It writes only the
namespace's remote-tracking refs and tags, from this pull's temporary refs,
never `refs/heads/*` and never a delivery marker. The client's `local` push
goes through the namespace's existing receive path. A refresh never writes a
pushed `refs/heads/*`, and the pool refuses a refresh push that does.

### 5. Out of scope here

`push` failing the whole command on a `-i` folder that was never a repository
(`noLocalRepository`) is a bug in the same command, with its own fix. Every
source is skipped unless `--source` names it. It needs no decision and is
delivered on its own.

## Alternatives rejected

- **Grant the box the credential again for a fetch (issue option 3).** This is
  what 253 §2 rules out. The box's program would then hold the user's
  upstream credential or a way to use it, and so would everything it runs.
  Sentinels (ADR 0031) don't help: Git over HTTPS to a forge would need the
  proxy to swap in the user's own credential, which the server would then have
  to keep, which 948 rejects ("Pull with a project secret the proxy swaps in").
- **`local` only: always fetch on the host and push.** It needs no change on the
  server or the pool, but a public remote's history would cross the
  client's link twice when the pool can fetch it once. A user who saved `lend`
  would then be ignored for refresh while honoured for create. Using the same
  order as a delivery keeps one rule.
- **A pool-side refresh only (`lend`, or anonymous).** It fails for anyone
  whose policy is `local` or who has no HTTPS helper. 253 makes `local` a
  supported choice, not a fallback.
- **A new verb (`discobox fetch` / `discobox refresh`).** `push` is already
  "get newer commits into a box": the issue's user reached for it, and its
  help is where they read why it didn't work. A second verb would split
  that job by where a source came from, which a user shouldn't need to know.
  The output names which source was pushed and which was refreshed.
- **Write the remote's branch as the namespace's `refs/heads/B`, so the box sees
  `discobox/B`.** For a local source that is where `push --branch B` lands.
  But for a remote source, `refs/heads/<checkout branch>` is the box's own
  branch, and the remote's branch of the same name would collide with it,
  which needs a lease that a refresh has no basis for. `origin/B` is the name
  the box's own work already uses for the remote's branch.
- **Give the box's `discobox` remote a second default refspec that mirrors
  `refs/remotes/origin/*`.** A plain `git fetch discobox` would then refresh
  `origin/*`. But in a box that also fetches a public `origin` directly, every
  `git fetch discobox` would rewind `origin/*` to the pool's possibly older
  copy. The explicit command in §1 updates `origin/*` only when asked.
- **Refresh on a schedule, or whenever a box starts.** That is 948's Deferred
  item, and its condition (creates routinely missing commits) hasn't been met.
  A box starting or waking with nobody there can't answer a credential
  question, and a pool-side schedule would have no credential to use for a
  private remote.
- **The box asks for a refresh itself.** An anonymous refresh only helps a public
  remote, which the box can fetch directly anyway. A credentialed one needs a
  person to answer for it, which is a different mechanism (see Deferred).

## Consequences

- A box from a private remote can take newer upstream commits:
  `discobox push BOX`, or `--branch staging`, then the in-box fetch in §1. A
  box from a public remote can too, without fetching `origin` itself.
- `discobox push` can now reach a remote and ask a credential question, which
  it never did before. For a non-interactive push of a mixed box, a private
  remote source under `ask` is skipped rather than failing a push that used to
  succeed.
- The `pull` route accepts calls with no credential, and calls on a delivered
  source. Its OpenAPI body and answer change with it.
- A refresh moves the cache's shared remote refs too, so a later create from
  the same remote on that pool starts closer to the remote's tip.
- What a refresh writes on the pool comes only from that pull or that push,
  and only into the remote-tracking refs and tags of the box's own namespace.
  545 §7's checks on a sandbox caller's pushes are unchanged.
- `cli/internal/cli/push.go`'s help, the user documentation for `push`, and the
  `sandboxes` and `gitcache` `DESIGN.md` files change with the implementation.

## Deferred

- **A box asks a person to refresh its source**, through the agent credentials
  protocol's request-and-wait (`discobox-access request`), so that an agent
  can get newer commits with nobody at the host's terminal beforehand.
  Revisit when agents routinely block on a refresh that a person then runs by
  hand.
- **Refreshing a local source's `refs/remotes/origin/*`** from the host's
  checkout of that source. The same staleness affects a box from a local
  checkout of a private repository. Today, `push --branch B` of an
  up-to-date local branch covers it. Revisit if users ask for `origin/*`
  itself to follow.

## Open implementation details

- The `pull` route's answer shape for a refresh (which refs moved and from
  where) and the exact status codes. §4 fixes only that an anonymous
  `needs_credential` is an answer, not an error.
- Whether the client's `local` refresh reuses `deliverRemote` directly or a
  shared fetch-and-push step factored out of it. The behaviour is §3's either
  way.
