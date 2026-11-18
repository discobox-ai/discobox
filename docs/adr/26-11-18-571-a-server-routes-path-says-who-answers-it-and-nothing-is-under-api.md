# 26-11-18-571 — A server route's path says who answers it, and nothing is under `/api`

- **Status**: Proposed
- **Date**: 2026-11-18
- **Relates to**: issue #1353; #1351 (`discobox admin api serve`, where a
  forwarded route reads `/v1/servers/NAME/api/api/projects/...`);
  [ADR 0118](0118-whoever-serves-an-api-is-strict-and-whoever-reads-it-is-tolerant.md)
  (how a reader and a server of different releases get along);
  [ADR 0140](0140-a-discobox-reaches-the-discobox-api-through-its-pool-with-a-fixed-role.md)
  (the sandbox role, whose route table names these paths).

## Context

`server/DESIGN.md` gives the server's paths a rule in passing: the root
(`/projects`, `/server`, `/peers`, `/providers`, `/ssh`) is the server's own
resources, `/api/projects/{p}/sandboxes/{s}/...` is forwarded into the sandbox,
and `/api/pools/...` is called by pool agents. Checked at `0972d519d`, the rule
holds for neither prefix.

The routes, from `api/openapi/server.yaml` and the hand-wired registrations in
`server/internal/server` (the full inventory is on #1353):

| Path today | Ops | Called by | Answered |
| --- | --- | --- | --- |
| root, in `server.yaml` | 111 | a client (CLI, console), or a sandbox through its pool (ADR 0140) | by the server |
| root, hand-wired: `…/sandboxes/{s}/http/{port}/…`, `git-repositories/…`, `git-origins/…`, `git-caches/…` | 4 | a client: a browser, git | forwarded: to a port in the sandbox, the sandbox agent's git, the pool's git cache |
| `/api/projects/{p}/sandboxes/{s}/…`, forwarded (`registerSandboxAgentTerminalRoutes`) | 36 | a client | forwarded to the sandbox agent through the pool agent |
| `/api/projects/{p}/sandboxes/{s}/{tcp,udp}/attach`, hand-wired | 2 | a client | forwarded to the sandbox agent |
| `/api/projects/{p}/sandboxes/{s}/harness-config-changes{,/discard,/reconsider,/save}` | 4 | a client | by the server |
| `/api/projects/{p}/sandboxes/{s}/…`, the sandbox agent's own (`status`, `status/wait`, `inventory`, `runtime-config` ×2, `autostop/confirm`, `autostop/resumed`, `cache/clear`, `sources`, `sources/{slug}/project-layer`, `meta`, `harness-config-capture`, `-ack`, `-reconsider`) | 14 | nobody, at the server: the pool agent calls them on the sandbox agent | `501` by the generated stub (`handlers/harness_terminals.go:47-116`, `harness_capture.go:58-66`) |
| `/api/projects/{p}/events`, `/api/projects/{p}/pools/{pool}/{console,logs,guest-image,audit/http/{x}/{artifact}}`, `/api/projects/{p}/sandboxes/{s}/export`, `/api/projects/{p}/sandboxes/import` | 8 | a client | by the server (hand-wired because they stream) |
| `/api/pools/{pool}/…` | 22 | a pool agent | by the server |
| `/healthz`, `/docs`, `/openapi.yaml`, `/shutdown`, `/ssh/connect` | 5 | operations, the CLI | by the server |

So `/api` means four things: forwarded to the sandbox agent, answered by the
server because it streams, called by a pool agent, and published but never
served. Three things follow from that:

- **`server.yaml` publishes 14 operations the server does not serve.**
  `sandbox.yaml` is generated as the `x-sandbox-agent` subset of
  `server.yaml`, so the sandbox agent's own API has to be written into the
  server's document to exist at all. `PATCH /projects/{p}/sandboxes/{s}/meta`
  (the server's record) and `PATCH /api/projects/{p}/sandboxes/{s}/meta` (the
  sandbox's meta file, answered `501` here) differ only by `/api`.
- **Authorization cannot tell them apart either.** `projectRoute`
  (`auth/authorizers.go`) strips `/api` before the sandbox role table matches,
  so the role's `PATCH sandboxes/*/meta` admits both. `PoolRouteAuthorizer`,
  `CarrierPoolAuthenticator` and `boundInventoryReports` key on the
  `/api/pools/` prefix.
- **`/api` reads as "the API"** in a client that already put the server under
  its own `/api` (#1351).

The sandbox agent itself serves its API at
`/api/projects/{p}/sandboxes/{s}/…`. The pool agent forwards the server's call
from `/api/project/{p}/pool/{pool}/sandboxes/{s}/<suffix>` to it
(`pool-agent/server/sandbox_proxy.go`). That hop is between two agents, and no
client addresses it.

## Decision

**A server route's path says who answers it. The root is the server's own.
`agent/` under a sandbox is the sandbox agent's API, forwarded. `http/` and
`git-*` under a sandbox are protocols forwarded as they are. `/pool-agent/` is
the pool agents' channel. Nothing is under `/api`, and there is no version
prefix. `server.yaml` publishes only what the server answers or forwards.**

### 1. The scheme

| Prefix | Means | Who calls it |
| --- | --- | --- |
| `/projects/…`, `/server…`, `/peers…`, `/providers…`, `/ssh` | answered by the server, streaming or not | a client, or a sandbox in its role |
| `/projects/{p}/sandboxes/{s}/agent/…` | forwarded to that sandbox's agent; its JSON API and its tunnels | a client, or a sandbox in its role |
| `/projects/{p}/sandboxes/{s}/http/{port}/…` | forwarded to that port in the sandbox, as HTTP | a browser, any HTTP client |
| `/projects/{p}/sandboxes/{s}/git-repositories/…`, `git-origins/…`, `git-caches/…` | forwarded as git smart HTTP, to the sandbox's worktree or the pool's cache | git |
| `/pool-agent/{poolId}/…` | the pool agents' channel to the control plane | a pool agent, never a client |
| `/healthz`, `/docs`, `/openapi.yaml`, `/shutdown` | the process, not the API | operations |

Under a sandbox, the segment after `{sandboxId}` decides. `agent`, `http` and
`git-*` are reserved for forwards, and every other segment is the server's.
Whether a route streams, or is hand-wired rather than generated, does not
change its path.

### 2. What moves

| Today | Becomes |
| --- | --- |
| `/api/projects/{p}/sandboxes/{s}/{execs…, exec-events, services…, tools, harness-hooks, reprovision, files/…}` (36) | `/projects/{p}/sandboxes/{s}/agent/<same suffix>` |
| `/api/projects/{p}/sandboxes/{s}/{tcp,udp}/attach` | `/projects/{p}/sandboxes/{s}/agent/{tcp,udp}/attach` |
| `/api/projects/{p}/sandboxes/{s}/harness-config-changes{,/discard,/reconsider,/save}` | `/projects/{p}/sandboxes/{s}/harness-config-changes{…}` |
| `/api/projects/{p}/sandboxes/{s}/export` | `/projects/{p}/sandboxes/{s}/export` |
| `/api/projects/{p}/sandboxes/import` | `/projects/{p}/sandboxes/import` |
| `/api/projects/{p}/events` | `/projects/{p}/events` |
| `/api/projects/{p}/pools/{pool}/{console, logs, guest-image}` | `/projects/{p}/pools/{pool}/{…}` |
| `/api/projects/{p}/pools/{pool}/audit/http/{x}/{artifact}` | `/projects/{p}/pools/{pool}/audit/http/{x}/{artifact}` |
| `/api/pools/{pool}/<action>` (22) | `/pool-agent/{pool}/<action>` |
| `/peer` | `/server/peer`, this server's own identity beside `/server` |
| the 14 sandbox-agent-only operations | no server path: they leave `server.yaml` (§3) |

Nothing else moves. That covers the other 110 root operations, `http/{port}`,
the three git routes, and the operational routes. `sandboxes/import` beside
`sandboxes/{sandboxId}` is unambiguous: a sandbox ID is generated (`sbx_…`),
and chi matches the literal segment first.

### 3. The sandbox agent's API is its own document

`sandbox.yaml` is the sandbox agent's whole API: what the pool agent calls,
and what the server forwards. It is no longer a subset of `server.yaml`.
`server.yaml` holds what a client may call on the server, so it has the
forwarded operations under `agent/` and none of the 14 that only the pool
agent calls. Each operation is written once, and the forwarded ones keep one
`operationId` and one schema in both documents. Which document is
hand-edited, and how the other is derived, is **open for implementation**:
either `sandbox.yaml` is canonical and marks what the server forwards, or the
server's forwards and the agent-only operations are separate sources that
`sandbox.yaml` is assembled from. `TestEveryPublishedOperationIsServedByTheServer`
(posted on #1353) holds the result: no published operation reaches the `501`
stub.

The sandbox agent's own paths (`/api/projects/{p}/sandboxes/{s}/…` on the
agent) and the pool agent's (`/api/project/{p}/pool/{pool}/…`, `pool.yaml`) do
not change. Only the pool agent and the server's lease client address them. A
sandbox keeps the agent bundle it is pinned to
([ADR 26-10-24-953](26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md))
until it is upgraded, so renaming them buys no client anything, and every pool
agent would have to speak both for as long as a sandbox not yet upgraded lives. **Deferred**: take it up when either
agent's contract next changes incompatibly for another reason.

### 4. Authorization follows the path, not a stripped prefix

The sandbox role table (ADR 0140 and its successors) names `agent/` routes
as `sandboxes/*/agent/…`, so `PATCH sandboxes/*/meta` admits only the server's
meta. `projectRoute` stops stripping `/api`. Pool authorization
(`PoolRouteAuthorizer`, `poolRuntimeActions`, `CarrierPoolAuthenticator`) and
the inventory body limit key on `/pool-agent/{poolId}/`.
`TestEveryPoolRouteInTheContractIsAllowlisted` checks that prefix.

### 5. No version prefix

There is no `/v1`. One server serves one contract. Releases of a CLI, a pool
agent and a server meet by additive change and tolerant reading (ADR 0118), by
the server manifest a release CLI is linked with, and, for a move like this
one, by aliases (§6). A version prefix promises a second version served beside
the first, which nothing plans. It would also put a segment in front of every
path that says nothing about the route, which is the problem with `/api`.
A program that puts servers under its own namespace, like `admin api serve`'s
`/v1/servers/{name}/api/`, versions that namespace, not the server's.
**Deferred**: take it up the first time a change cannot be made additively,
when both shapes have to be served for longer than an alias's release.

### 6. Old paths answer as aliases

- **The server serves both from the release that moves a route.** Each old
  path is rewritten to its new one before authentication and authorization
  run, so an alias reaches exactly what the new path does and is authorized
  as the new path. Aliases are not published in `server.yaml`. The 14
  sandbox-agent-only operations are not aliased: they answer `404` instead of
  `501`, and no caller used them.
- **A caller moves after the server.** A pool agent is never newer than its
  server, because the server brings its pools onto its own agent, so it moves
  in the release that adds the new paths. The CLI and console can meet an
  older registered server, so they move no earlier than the release after.
  In-box callers, including the CLI inside a sandbox through
  `api.discobox.internal`, are the same CLI.
- **An alias is removed** no sooner than the release after every in-repository
  caller has moved, and at least one release after the server first served the
  new path. The removal is its own change, named in that release's notes.

### 7. Callers to update

- `api/openapi/server.yaml`, `sandbox.yaml`, `api/internal/gensandboxopenapi`,
  and the generated `api/gen` and `api/sandboxgen`.
- Server: `internal/server` (`sandbox_agent_terminals_proxy.go`,
  `sandbox_tunnel_proxy.go`, `sandbox_events.go`, `sandbox_transfer.go`,
  `pool_console.go`, `pool_logs.go`, `pool_guest_image.go`,
  `pool_http_audit.go`, `inventory_report_limit.go`, the startup handler's
  route set), `internal/handlers` (the `501` stubs go), `internal/auth`
  (`middleware.go`, `authorizers.go`, `authenticators.go`).
- CLI and console: `cli/internal/cli` (`events.go`, `pool_console.go`,
  `pool_logs.go`, `pool_guest_image.go`, `audit_http_body.go`,
  `sandbox_transfer.go`, `cp_transfer.go`, `port_tunnel.go`,
  `sandbox_execs.go`, `identity.go` for `/peer`), and every generated-client
  call, which follows `api/gen`.
- Pool agent: `http.go`, `proxyagent/credentials.go`,
  `proxyagent/secrets.go`, `pkg/poolassets`. Its calls into the sandbox agent
  (`statuspoll.go`, `statuswait.go`, `inventorypoll.go`, `sandboxruntime/`)
  do not change (§3).
- Docs and tests: `DESIGN.md`, `server/DESIGN.md`,
  `server/internal/auth/{DESIGN,REVIEW}.md`, `server/internal/resources/**/DESIGN.md`,
  `cli/DESIGN.md`, `pool-agent/DESIGN.md`, the `verify-recipes` skill,
  `test/bats/terminal_preparation.bats`, `test/bats/worktree_git.bats`, and
  the server, auth and CLI tests that name these paths.

## Alternatives rejected

**Put everything under `/api`.** Then `/api` would mean "the API" and nothing
narrower, the root would hold only the process's routes, and 110 operations
would move instead of 72. A path would still not say who answers it, and
every program that mounts the server under its own `/api` gets `/api/api`.

**Put every forward under `agent/`, git and ports included.** One segment would
then mean "forwarded". But a port's URL and a git remote are stored outside the
server: in a clone's remotes, a browser's history, and the URLs `discobox`
prints. They are protocols, not the agent's JSON API, and the pool agent, not
the sandbox agent, answers a port and the git cache. Their own segments
already say what they are.

**Keep the 14 sandbox-agent-only operations in `server.yaml`, marked.** That
is the smallest change, and the triage offered it. The server's document
would still advertise operations the server refuses, a generated client for
the server would still have methods that cannot succeed, and the stub would
still be needed to satisfy the generated interface.

**Name the pool channel `/pools/{poolId}/…`.** That reads as the pool resource,
which is `/projects/{p}/pools/{pool}`, and says nothing about the caller. The
channel is the pool agent's, and its prefix names it.

**Also rename the sandbox agent's and pool agent's own paths now.** Deferred
in §3: no client addresses them, and a sandbox's pinned agent would keep both
paths live in every pool agent until that sandbox is upgraded.

**Move without aliases.** #1353 requires that an older CLI or pool agent keep
working against a newer server. Pools are upgraded after their server, and a
registered remote server can be older or newer than the CLI.

## Consequences

- `server.yaml` and the server's behaviour agree: every published operation is
  served, and the test from #1353 holds it.
- A reader can tell from a path who answers it, and the sandbox role table can
  tell the server's meta from the sandbox's.
- For about two releases the server answers each moved route at two paths.
  The rewrite sits in front of authorization, so there is one authorization
  decision per route, not two.
- `discobox admin api serve` exposes a forwarded route as
  `/v1/servers/NAME/api/projects/{p}/sandboxes/{s}/agent/…`.
- The sandbox agent's and pool agent's wire paths keep their `/api` until §3's
  deferral is taken up.
