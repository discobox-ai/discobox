# 26-10-23-948 — A pool pulls a cached source first, and a client supplies what the pull could not

- **Status**: Accepted
- **Date**: 2026-10-23
- **Supersedes**, for sandboxes created after it:
  [26-10-10-545](26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md)
  §1's "the pool never fetches from a remote and never holds a credential for
  one" and "a create that names a Git source and has no client parks", §3's
  order of delivery for a remote-URL source, and its rejection of "the pool
  fetches with a credential the client lends" and "the pool fetches
  anonymously when the remote is public".
- **Relates to**:
  [0128](0128-a-private-remote-source-is-fetched-with-a-credential-the-client-lends.md)
  (rejected; §8 here takes up its lending mechanics, not its choice of
  delivery),
  [26-09-24-005](26-09-24-005-attaching-to-a-discobox-awaiting-its-source-delivers-it.md),
  [26-09-24-630](26-09-24-630-a-discobox-delivers-the-source-of-the-discoboxes-it-creates.md),
  [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md).

## Context

ADR 26-10-10-545 delivers every source by a client: the client pins what the
pool's per-remote cache already holds and pushes the rest, and for a
remote-URL source it first fetches the rest from the remote into its own
cache. The pool never fetches. That bought one delivery path, no credential
anywhere but the client, and a cache that makes every create after the first
a delta.

It left three costs, two of them named in its own Consequences and Deferred:

- **The first create for a remote moves its history twice**, remote to client
  to pool, through the client's link. 545 deferred streaming the pack through
  the client "when the first create for a large repository is what people
  wait on". A pool usually sits on a better link to a forge than a laptop
  does, and a pool fetch moves the history once.
- **A remote-URL create needs a client until its source is delivered.** A
  public repository cannot be started detached, from another box without its
  own fetch, or from anything that is not the CLI — although nothing about it
  needs a credential.
- **A sandbox client moves the history through its own proxy.** A discobox
  creating discoboxes from a remote URL fetches into its sandbox and pushes
  back to the pool it runs on.

545 rejected the pool fetching anonymously because it "is a second delivery
chosen by probing a remote", and rejected lending a credential (ADR 0128)
because "a user's credential transits the server and the pool, and a create
becomes a credential-bearing request". This ADR keeps one delivery and no
probe-time choice: the cache delivery stays `cache`, the pool's pull is one
more way the cache is filled before a client is asked, and a client supplies
whatever the pull could not — by the push it already has, or by lending a
credential for one request it makes itself. The create is never
credential-bearing; the credential travels only in a request a client makes
in answer to a recorded failure, while a person is there to answer it.

## Decision

### 1. Delivery stays `cache`; the pool pulls first

`GitSource.Delivery` keeps the one value 545 gave it. Nothing new is chosen at
create, and the server still refuses a create that names a delivery.

For every source whose cache key is a remote (545 §2), **the pool agent pulls
that remote into the source's cache when it is asked to create the sandbox**,
anonymously, before and alongside anything a client sends:

- It fetches the remote's branches and tags into the cache's refs outside
  every namespace (545 §5), and the source's checkout commit if no branch or
  tag reaches it.
- If the cache then holds the checkout commit, **the pool pins the
  namespace itself** with the refs 545 §3's table names — the branch at the
  commit, `refs/remotes/origin/*` and `refs/tags/*` from what it fetched —
  and the source is delivered. No client is involved.
- A source with no remote (545 §2's second and third rows) is not pulled. A
  client delivers it as today.

The pull runs as Git, with no credential helper, no terminal prompt
(`GIT_TERMINAL_PROMPT=0`) and no SSH agent, bounded by a stall timeout. It
writes refs outside every namespace and a namespace's refs only when it pins,
so it needs nothing from a concurrent client push but object writes Git
already makes safe. `gc` keeps its exclusive hold (545 §5); a pull waits for
it like a pin.

### 2. The pool pulls over HTTPS

The pull's URL is the source's remote when that is an HTTPS URL, and
otherwise **`https://` plus the remote's normalized cache key** (545 §2's
host and path). An SSH remote such as `git@github.com:owner/repo.git` is
pulled from `https://github.com/owner/repo`. A host with no HTTPS Git
service fails the pull as unreachable (§3), and a client pushes.

The HTTPS spelling is what a credential is filled for (§5): Git's credential
helpers answer HTTP only, and a lent SSH key or a forwarded agent would let
the pool sign as the user for as long as it held it.

### 3. A pull that falls short is recorded, per source, with what would fix it

The pool reports each pulled source's result to the control plane in its
create answer, and the server records it on the source's delivery state:

| The pull | Recorded | What a client may do |
| --- | --- | --- |
| Holds the commit, and pinned | delivered | nothing |
| Refused for authentication (HTTP 401, or 403 with a Git auth challenge) | `needs_credential`, with the `protocol`, `host` and `path` of the URL asked | push, or lend a credential (§5) |
| Reached the remote, which does not have the commit | `missing_commit` | push |
| Could not reach a Git service, or stalled | `unreachable` | push |

The sandbox stays in `awaiting_source` while any source is owed, as it does
today. **Delivery is recorded per source**: the sandbox resumes once every
source is delivered, whether the pool pinned it or a client completed it, and
`SourceDeliveredAt` records the last of them. A client's
`complete-source-push` names only the sources it delivered; the server
refuses one that names a source already delivered at another commit, and
accepts one that repeats a delivery at the same commit.

### 4. A client pushes what it holds, and lends a credential only when it does not

A client waits on the recorded result of each source it may deliver, except
that **a client that already holds a source's checkout commit pushes it
without waiting**. Pushing is preferred whenever the client has the commit:
it needs no credential to leave the client and no remote to be reachable,
and 545 §3's negotiation sends only what the cache — including whatever the
pull has landed — still lacks.

| The source | The client |
| --- | --- |
| A local checkout | holds the commit: pins and pushes as 545 §3 says, at once |
| A remote URL whose commit is in the client's cache of that remote | pushes from that cache, at once |
| A remote URL the client does not hold, pull delivered | nothing |
| …recorded `needs_credential` | lends a credential (§5) if its user agrees; otherwise fetches and pushes as 545 §3 says |
| …recorded `missing_commit` or `unreachable` | fetches and pushes as 545 §3 says; if the remote does not have the commit there either, the create fails with that reason |

A local checkout's unpushed commits and dirty snapshot exist only on the
client, so a local source always needs its client; what the pull adds there
is the remote's published history, which shrinks the push.

**Any client may answer**, not only the creating one: a later `discobox
attach` or `start` of a sandbox awaiting its source delivers it
(ADR 26-09-24-005), and now that includes answering `needs_credential`. A
detached create of a private repository waits for one, and fails when
`awaiting_source` times out, as an undelivered source does today.

### 5. A credential is lent in a request the client makes, for that request

Lending takes up ADR 0128 §2's mechanics:

1. The client asks Git for a credential: `git credential fill` with the
   recorded `protocol`, `host` and `path`, so whatever supplies the user's Git
   over HTTPS — a keychain, Git Credential Manager, `gh auth setup-git`,
   libsecret, a store — supplies this, and a helper keyed by path
   (`credential.useHttpPath`) gets the path. With no helper that answers, Git
   prompts on the terminal itself.
2. It calls `POST .../sandboxes/{s}/sources/{slug}/pull` with the username and
   password. The server forwards the request to the pool agent, which pulls
   as §1 says **inside that request**, with the credential reaching Git only
   through the process environment (a `GIT_ASKPASS` helper reading an
   inherited variable). It is never written to the cache's configuration, a
   credential store, a resource, an event or a log, on the server or the
   pool, and neither keeps it after the request returns.
3. An authentication failure answers 401; the client runs `git credential
   reject` and asks again while its user is there. A pull that pins answers
   with the delivery, and the client runs `git credential approve`.

**A credential is lent only with its user's consent.** An interactive client
asks before running `fill` — "github.com/owner/repo needs credentials to
pull on the pool. Use your Git credentials? [Y/n]" — and a non-interactive one
lends only when told to by a flag or a configuration setting; otherwise it
fetches and pushes, and nothing leaves the client.

**A sandbox never lends.** A discobox creating discoboxes holds sentinels, not
credentials (ADR 0031), and a sentinel is worthless outside its proxy. It
fetches and pushes as 545 §3 says, through its own proxy.

### 6. What a pull may reach is what the create named

The pool pulls only the remote the source's cache key names, and only into
that cache. A sandbox caller's create names its own remote (545 §7), so a
sandbox can make its pool pull any public HTTPS repository into the cache
for that repository's key — what it could already do by pushing the same
history, with the bytes moving once instead of through its proxy. 545 §7's
check on a sandbox's pins and pushes is unchanged: the pool's own pin for a
sandbox caller's source is made only from what the pull fetched for it.

A pool's pull goes out from the pool host, as its proxy's traffic does; a
pool whose host cannot reach a forge records `unreachable` and its clients
push.

## Alternatives rejected

- **Probe at create and choose a delivery (ADR 0128 §1).** The client would
  report whether the remote reads anonymously and the server would choose
  `clone`, `fetch` or `push`. It brings back the choice 545 removed, and the
  client's probe says nothing about the pool's reach. Here the pull is always
  tried and its recorded result is the probe.
- **The pool asks a client for a credential through a callback during the
  pull.** It needs a channel from the pool to one particular client, which
  0128 and 545 both rejected, and a prompt that fails lands in the pool's
  reconcile. Recording the failure and having a client answer it in its own
  request gets the same "pull first, credential on demand" behaviour with no
  reverse channel.
- **Lend the credential with the create.** A create is persisted and
  reconciled asynchronously, so the credential would be stored until the pool
  reached it (0128's own rejection).
- **Prefer lending over pushing when the client holds the commit.** It would
  move the history once from the forge, but it sends a credential through the
  server and pool when no credential is needed: the push negotiates against
  what the pull already landed, and costs only what the cache lacks.
- **Have the client wait for the pull before pushing a local source.** A
  local source always needs its client for unpushed work, and an anonymous
  pull of a private remote is a guaranteed failure; waiting would make every
  local create of a private repository slower by that round trip.
- **Pull over SSH with a lent key or a forwarded agent.** A key or agent on
  the pool can sign anything for as long as it is held; HTTPS credentials are
  scoped by the forge and filled by Git for one URL.
- **Pull with a project secret the proxy swaps in.** The server would own a
  user's Git credential, usable by every later create on the pool — the
  opposite of lending (0128's rejection, unchanged).

## Consequences

- A create from a public HTTPS-reachable remote needs no client: it can run
  detached, from a box that does not fetch it first, or from a non-CLI
  caller. 545's "a remote-URL create needs a client until its source is
  delivered" holds only for private remotes and local sources.
- The first create for a remote the client does not hold moves the history
  once, forge to pool, when the pull or a lent credential succeeds.
- A private HTTPS remote can be delivered with a credential that transits the
  server and the pool in memory, for one request, with the user's consent.
  That is new exposure on the control plane, bounded by the request, and the
  pull route is the one server route whose body carries a credential.
- The pool depends on a remote's speed and reachability again, but only for
  speed: a pull that fails or stalls is a recorded result a client answers by
  pushing.
- A private repository pulled with one member's credential is in a cache
  every member of the project shares, as one member's push already is
  (545 §2).
- Delivery is recorded per source, and `complete-source-push` covers only the
  sources a client delivered. The sandboxes `DESIGN.md`'s "completion is one
  report for the whole sandbox" changes with it.
- The pool agent gains a pull and a self-pin path, and its create answer
  carries each source's pull result; the CLI gains the credential prompt, the
  pull request, and a wait on recorded results for remote-URL sources.

## Deferred

- **Pulling for a source whose cache key is a client directory.** It has no
  remote to pull. Revisit if local sources without a remote become common.
- **Refreshing a cache's remote refs on a schedule**, so a later create finds
  the remote's newest commits already there. Revisit when creates from a
  remote routinely miss on commits the pool's last pull predates.
- **A non-HTTPS pull** (a host with SSH-only Git, `git://`). Revisit if such a
  host is a project's primary remote.
