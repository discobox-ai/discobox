# 0001 — Sandbox origin and remote source push

- **Status**: Accepted
- **Date**: 2026-07-16

## Context

The CLI's UX is converging on "run against the current directory". `disco ls`
answers "what did I start from here?" by filtering on `Sandbox.SourceRoot`, a
value derived from the primary `GitSource`: the local Git repository root path
for a local source, or the remote URL for a remote one.

That conflates two unrelated jobs in one field:

1. **Repo identity** — what to materialize in the sandbox.
2. **Client project location** — where the request came from.

They coincide only while the client and the server are the same machine. Against
a remote server, `/home/darren/src/disco2` is a string the server cannot
interpret, collides across hosts and users, and is wrong when the same repo is
checked out on two laptops.

Local sources are also assumed to be reachable. `gitSourceCloneURL`
(`worker-agent/sandboxruntime/runtime.go`) passes `GitSource.LocalDirectory` to
git **as a clone URL**; the bind mount exists only so git can reach that path.
The sandbox therefore already receives its source over git — the bind is the
current transport for the objects, not the mechanism. A provider that cannot
reach the client's filesystem has no path to the source at all.

## Decision

### 1. Record client origin explicitly

Add client-declared origin metadata to `Sandbox`, set at create, immutable
after, never used to materialize source.

```go
// Origin identifies the client and project directory a sandbox was created
// from. Recorded verbatim; inert with respect to materialization.
type Origin struct {
    HostID      string `json:"hostId"`         // stable generated client identity
    Hostname    string `json:"hostname"`       // display only, may change
    ProjectPath string `json:"projectPath"`    // abs Git repo root on that host
    User        string `json:"user,omitempty"` // display only
}
```

On `Sandbox`: `Origin *Origin` (JSON-serialized) plus an indexed derived scalar
`OriginKey = sha256(hostId + "\x00" + projectPath)`. `ListSandboxes` filters on
`originKey`.

`ProjectPath` is the Git repository root — matching how Claude Code defines a
project — falling back to the working directory outside a repo, so `disco ls`
still works there.

`SourceRoot` is **retained**. The two answer different questions: `originKey`
for "sandboxes I started from this directory", `sourceRoot` for "every sandbox
against this repo, from anyone" — the latter being meaningful for remote URLs
and increasingly useful with a shared server.

### 2. HostID is generated and persisted, never derived

`host_<crockford16>` via the `id` package, stored at
`<xdg.ConfigHome>/discobox/host-id` (matching `server/internal/config`), written
atomically (temp file in the same directory, `0600`, rename). A file that fails
to parse is treated as absent and regenerated. `DISCOBOX_HOST_ID` overrides.
Resolved once per process.

### 3. Local bind support is a provider-instance capability

`LocalSourceBind bool` on the sandbox provider, exposed on the provider read
endpoint. It is a property of the provider instance, not the server: one server
may have both a Docker provider (can bind) and a DigitalOcean provider (cannot)
configured at once.

The **server** decides bind vs. push — the client always posts
`Source.LocalDirectory` plus `Origin`. The condition is:

```
bind is possible ⟺ provider.LocalSourceBind && origin.HostID == server's own host ID
```

A Docker provider on a *remote* server can bind, just not to the client's files.
Without `HostID` the server cannot distinguish these cases — which is why origin
must land before this.

### 4. Unreachable local source becomes a phase, not an error

New phase `awaiting_source` between `provisioning` and `starting`:

```
pending → provisioning → awaiting_source → starting → running
```

It sits after provisioning because a push needs a live worker-agent and an
initialized repo to land in. On reaching it, the client pushes the source into
the sandbox's repo, then calls a continue endpoint naming the commit/ref to
check out; the worker-agent proceeds with its normal checkout.

Because the direction of the git transfer is inverted but the transfer itself is
unchanged, `applyWorkspaceSnapshot`, the `refs/discobox/run/<id>` snapshot refs,
and the checkout logic are transport-agnostic and unaffected. Dirty mode gets
*simpler* on this path: the client pushes the snapshot commit directly instead
of shipping a patch to re-apply.

Supporting decisions:

- **Push is proxied through the API server** (`git-receive-pack` over the
  existing worker-agent connection, reusing sandbox auth). Direct
  client→sandbox push would require reachability into a private network.
  **This transport already exists** and is what makes this decision cheap:
  `worker-agent/githttp` serves `git http-backend` with `http.receivepack=true`
  and `receive.denyCurrentBranch=updateInstead` (so a push to the checked-out
  branch updates the working tree), `worker-agent/server/sandbox_git.go` routes
  it, and `server/internal/server/sandbox_git_proxy.go` proxies
  `/projects/{p}/sandboxes/{s}/git-repositories/{slug}.git/*` with read/write
  scopes derived from the requested service. `GitSource.Slug` is the repository
  ID, so the primary source is already addressable at `primary.git`. No new
  contract, proxy, or worker surface is required.
- **Continue is an explicit call.** A push is N refs arriving with no statement
  of intent, not a completion signal. Explicit continue makes a partial or
  aborted push a no-op rather than a corrupt start.
- **`awaiting_source` times out to `failed`.** It is the only phase that waits
  on an external actor, so it is the only one that can park a VM indefinitely.

## Alternatives rejected

**Derive HostID from the machine.** Every source fails in a way that matters:

| Source | Why not |
| --- | --- |
| Hostname | Not unique (`localhost`, duplicate laptop names); changes on rename/VPN, silently orphaning that host's sandboxes |
| MAC address | Randomized Wi-Fi MACs; `docker0` appears/disappears; choosing "the" interface is arbitrary |
| `/etc/machine-id` | Absent on macOS; **baked into container images**, so every container from one image reports the same identity — a correctness bug, not cosmetic |
| Hardware UUID | Platform-specific, often root, identical across VM clones |

All answer "what machine is this?" The requirement is only "which client's
sandbox list is this?" — unique and stable, not physically meaningful.

**Replace `SourceRoot` with `OriginKey`.** Loses the cross-machine "all
sandboxes against this repo" view, which becomes more valuable, not less, with a
shared server.

**Server-wide local-bind flag.** Wrong the moment one server has both a local
Docker provider and a cloud provider configured.

**Reject creates with an unreachable local source.** Rejecting is the status quo
in effect; the whole point is to make the case work. The sandbox is a git repo
already, so the source has a natural path in.

**Client-side bind/push decision.** Only the server knows which provider will be
resolved and whether it shares a host with the client. Capability is still
exposed for early client-side messaging, but is not the decision point.

**Combine generated ID with a weak machine signal** so a synced `host-id`
self-heals. Reintroduces instability on rename, which is the worse failure.
Accepted consequence below instead.

## Consequences

- Origin must land before the capability check; the check is unimplementable
  without `HostID`.
- **Every push-path sandbox pays a full-history push.** A fresh sandbox's repo
  is empty, so there is no common history to negotiate against; incremental push
  only helps a second push to a warm sandbox, which is not the common case.
  Large repos will feel this.
  - *Deferred*: a per-repo bare cache repo on the server (clients push
    incrementally into it, sandboxes clone from it locally at provision time).
    Adds server-side storage and a GC story. **Revisit if** measured push time
    against the largest real repo is unacceptable — not before.
- `host-id` lives in `$HOME`, so identity is per-user-per-machine and follows a
  synced home directory. Dotfile sync (chezmoi, Dropbox) or
  `docker run -v ~/.config:...` will replicate it and merge sandbox lists.
  Mitigation is documentation plus `DISCOBOX_HOST_ID`.
- Existing sandboxes have no origin. Given a disposable DB this is a non-issue;
  `origin` is nullable and such sandboxes simply do not appear in `disco ls`.
- `awaiting_source` is a new phase in a public enum: `SandboxPhases`, the
  OpenAPI enum, and CLI rendering all move together.

## Work order

1. `id.PrefixHost`; `cli/internal/origin` (host-id file, `Resolve`).
2. `Origin` + `OriginKey` on `Sandbox`; `origin_key` index; create/list API;
   `disco ls` filters on `originKey`.
3. `LocalSourceBind` capability; server-side bind-vs-push decision.
4. `awaiting_source` phase, continue endpoint, timeout, and CLI push. The
   transport exists (see above); the real work is that the sandbox's repository
   must be `git init`-ed when there is nothing to clone from, because
   `GitRepositoryPath` requires it to exist before a push can land.

On completion, update the live design docs to describe the resulting system:
root `DESIGN.md` (source materialization paths), `server/DESIGN.md` and
`cli/DESIGN.md` as affected. This ADR is not edited afterward.
