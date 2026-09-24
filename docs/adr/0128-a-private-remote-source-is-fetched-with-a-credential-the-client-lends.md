# 0128 — A private remote source is fetched with a credential the client lends

- **Status**: Proposed
- **Date**: 2026-09-18
- **Relates to**: [0001](0001-sandbox-origin-and-remote-source-push.md),
  [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
  [0045](0045-a-directory-with-no-repository-is-delivered-by-push.md),
  [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md),
  [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md), and
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md).

## Context

`-C` takes a repository URL, and now GitHub shorthand for one (`owner/repo`,
`github.com/owner/repo` → `https://github.com/owner/repo.git`). A remote source
is delivered by clone: `sourceNeedsPush` returns false for any source without a
`LocalDirectory`, and pool-agent's `materializeGitSource` clones the URL
directly (`gitSourceCloneURL`).

That clone runs with no credential. Pool agents are given none, deliberately:
a credential on the pool host outlives the create that needed it, is shared by
every sandbox on the pool, and belongs to a user the pool does not represent.
So a private repository fails, and it fails late. The client's own `git
ls-remote` (`resolveRemoteGitRef`) succeeds with the user's credentials, the
create is accepted, and the clone fails in the pool agent, in a reconcile loop
nobody is watching.

The model we want is `docker pull`'s: the credential stays with the client and
is lent for one operation. Two pieces are already in place:

- **A parked delivery phase.** A push-delivered source parks its sandbox in
  `awaiting_source` until the client completes it (ADR 0045, ADR 0055), so the
  client is known to be present at create and nothing runs before the source
  settles.
- **A pool-side origin.** Every push-delivered source has a bare repository on
  the pool host that the worktree is cloned from and that the sandbox's `origin`
  points at (ADR 0058). Remote sandboxes clone from the same place (ADR 0126
  §4).

What is missing is a way to fill that origin from a URL the pool can reach but
not read.

## Decision

### 1. The client reports whether the URL reads anonymously; the server decides

Before create, the client runs `git ls-remote` against a remote source twice as
needed: first anonymously (no credential helper, `GIT_TERMINAL_PROMPT=0`), then
with its normal configuration. A source that reads only with credentials is
reported as a fact on the request — `GitSource.RequiresCredential` — the same
way ADR 0045 §3 has the client report `NoLocalRepository`. The client still may
not request a delivery mode; the server derives it:

| Source | Delivery |
| --- | --- |
| URL, reads anonymously | `clone` (unchanged: pool-agent clones the URL) |
| HTTPS URL, `RequiresCredential` | `fetch` (§2) |
| Any other URL, `RequiresCredential` | `push` (§4) |

A public repository costs one extra anonymous `ls-remote` and nothing else.

### 2. `fetch` delivery: the client lends a credential for one fetch

A `fetch`-delivered source parks in `awaiting_source` exactly as a
push-delivered one does, with the same bare pool-side origin (ADR 0058 §1). The
client then:

1. Obtains the credential with `git credential fill` for the URL's host, so
   whatever supplies the user's git (`gh auth setup-git`, a keychain, Git
   Credential Manager) supplies this.
2. Calls a new endpoint,
   `POST /projects/{p}/sandboxes/{s}/sources/{slug}/fetch`, with the
   username and password. The server forwards it to pool-agent, and pool-agent
   runs `git fetch <url> <checkout commit>` into the source's origin **inside
   that request**. The credential reaches git only through the process
   environment (a `GIT_ASKPASS` helper reading an inherited variable). It is
   never written to the origin's config, a credential store, or a log, and
   neither the server nor pool-agent keeps it after the request returns.
3. On a git authentication failure the endpoint answers 401 and the client asks
   again (`git credential reject`, then `fill`), while the user is still at the
   terminal. On success the client runs `git credential approve` and calls
   `complete-source-push` with the checkout commit, unchanged. From there the
   source is materialized like any push-delivered one: the worktree is cloned
   from the origin, and `origin` is `/.discobox/origins/<slug>`.

The origin's recorded URL stays the real one, so a later credentialed fetch
from inside the sandbox goes to the right place. That fetch is the
agent-credentials protocol's job (ADR 0031), not this one's.

### 3. The credential transits the control plane in memory

The client reaches pool-agent only through the server, so the lent credential
passes through both, in memory, for one request. This is the `docker pull`
trust level: the daemon sees the registry credential while it pulls. The pool
host already sees cleartext secrets transiently whenever the proxy swaps a
sentinel (ADR 0031 §3), so this does not widen the pool's boundary. The control
plane's exposure is new, and it is bounded by the request: nothing persists it,
and it appears in no resource, event, or request log.

### 4. Everything else is fetched by the client and delivered by push

An SSH URL, or any URL whose credential is not a username and password, cannot
be lent this way. For those the client fetches the checkout commit into a
per-user cache of bare repositories and delivers it with the existing push
path (ADR 0058 §4). No credential leaves the client. The data crosses the
network twice, so this is the fallback, not the default.

## Alternatives rejected

- **Client-side fetch and push for every private repository.** No credential
  ever leaves the client, and every URL scheme works. It was rejected as the
  default because every create moves the whole history twice — git host →
  client → pool — through the client's uplink, and each sandbox gets a fresh
  origin, so a client-side cache makes only the first leg cheap. It stays as
  §4's fallback.
- **Give the pool agent a credential, or store it as a project secret swapped
  by the proxy.** The server would own the user's git credential, and it would
  be usable by every later create on the pool — the opposite of lending.
- **A credential callback during the clone.** Pool-agent runs git with a
  credential helper that calls back to the attached client. It moves the same
  secret through the same hops as §2 but needs a reverse channel from the pool
  to a specific client, and a failed prompt lands in the pool's reconcile
  instead of the client's request.
- **Send the credential with the create request.** It is the literal `docker
  pull` shape, but create is persisted and reconciled asynchronously, so the
  credential would have to be stored until the pool gets to it.

## Consequences

- A private HTTPS repository works from `-C` with no setup beyond the user's
  own git credentials, and a bad credential is reported to the user who can fix
  it.
- A third delivery mode, `fetch`, joins `clone` and `push` on `GitSource`, and
  every stage that branches on delivery has to place it. Almost everything
  treats it as `push`: it parks, has a pool-side origin, and resumes on
  `complete-source-push`.
- A `fetch` or `push` source for a URL needs its client at create, as a local
  push-delivered source already does. A detached create returns only after
  delivery completes, as today.
- The fetch endpoint is the one server route whose request body carries a
  secret; its handler and pool-agent's must be excluded from request-body
  logging and error wrapping that would echo it.

## Deferred

- **A direct client-to-pool path for the credential**, keeping it off the
  control plane entirely. Revisit when clients can reach a pool agent without
  the server in the path.
- **A pool-shared object cache** for repeated creates from one URL, so a second
  sandbox fetches only what changed. Revisit when fetch time dominates create
  for large repositories.
