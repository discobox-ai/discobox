# Concepts

This page describes the **v0.19 prerelease** line.

## Boxes and sources

A discobox is a disposable environment with its own writable disk, terminals,
services and copy of your source. You can give it passwordless sudo and
nested Docker without giving it your host checkout. Its name is a display
label; its ID is unambiguous. Names and tags make parallel tasks easier to find.

`discobox run SOURCE [PROMPT...]` creates a box. A source can be a local
repository, repository URL, or `OWNER/REPO`. A local source starts at the
checked-out commit. Uncommitted changes are included only with
`--include-dirty`; they arrive as uncommitted changes. Additional sources can
be supplied with `-i`. At a terminal, a directory without Git asks whether to copy it. Without a
terminal, the directory is copied automatically.

The box fetches its source through a read-only origin. Your host's files
stay separate. `discobox push BOX` delivers your local commits into an
existing box; it does not bring the box's changes home. Commit and push the
agent's result to a Git host, then review and merge it in your normal workflow.

## Harnesses

A harness packages an agent CLI and the instructions for launching it.
Included harnesses include Claude Code, Codex, OpenCode, GitHub Copilot, omp,
pi and a shell. `discobox configure` opens the harnesses screen, where you
review settings and logins. `discobox run -H HARNESS` selects one for a task.

Saved configuration is applied when a box is provisioned. Changes made in
one box are reviewed before being saved back as shared harness settings.
The console's `H` action opens that review; inspect its changes before saving.

## Providers and pools

The server is the control plane. A provider supplies runtime capacity; a
pool uses that provider to run boxes and share downloads and build caches.
The CLI can connect to a local or remote server. The same command on your
machine can therefore run a box on a different host. A project groups its
boxes, harnesses, pools and secrets.

Use `discobox admin provider catalog` to inspect the provider types offered
by the connected server. Provider-specific flags for create and update are
**runtime definitions**, supplied by that server's catalog, rather than
compiled Cobra flags. They can differ between servers and versions. Inspect
both the catalog and the matching help before configuring a provider:

```sh
discobox admin provider catalog -o json
discobox admin provider create --help=docker
discobox admin provider update --help=docker
```

The [CLI reference](cli.md) exports every visible static command and flag,
including the generic `--type`, `--config` and `--secret` options. It does not
claim to snapshot runtime provider fields. Use `--help=TYPE` on the same
server you will modify for their names, required values and descriptions.
Secrets are supplied separately from provider config. Never put a token in
a documentation snapshot or a catalog fixture.

## Lifecycle and storage

Running, starting, stopped, archived and error states appear in the console.
Stopping releases runtime resources while retaining the box's persistent
workspace. Starting resumes or boots it; terminals may be preserved by a
freeze or relaunched after shutdown. Leaving an attached window is different
from stopping: the box and its sessions keep running.

Idle boxes stop automatically. Active terminal output, connections and
explicit keepalive leases affect idleness; a background server alone does
not guarantee that a box stays awake. The pool determines the idle timeout.

Archiving hides a box and starts its retention lifecycle. Unarchive can
recover it while retained; purge permanently destroys its data. Push useful
work to a remote before discarding a box. Shared caches are performance
storage, not trusted copies or backups. Never place a secret in a cache.

## Credentials

Managed secrets stay outside the sandbox. A box receives a placeholder
(sentinel); the pool proxy substitutes the real credential only for allowed
requests. A binding chooses who may use a secret, which routes it allows,
and how long it lasts. Project, harness and box bindings serve different
scopes. Raw credentials, when explicitly allowed, are a different grant:
they are readable by the box.

An agent can request access it lacks. Review the destination, methods,
paths, lifetime, and the routes behind any named role. A request may ask for
use, delegation to children, or both. Decide each part; approving use does
not imply approving delegation. The console marks boxes with pending
requests; open the request action to approve or deny it. Do not approve a
role just because its name sounds appropriate.

Outbound requests are subject to egress policy and auditing. Isolation and
credential routing do not make agent-written code trustworthy: review code
and results before merging or running them on your machine.

## Services, ports and tools

Repositories can declare services under `.discobox/services`. They start at
boot and their output appears in service tabs. A service that exits remains
exited. Listening ports are discovered or declared and forwarded while the
client is connected; the console shows where to reach them.

Tools under `.discobox/tools` open editors or other programs for a box.
`discobox tools ls` lists available tools. Repository tools run inside the
box; only user-owned tools may request execution on the user's host.

## Private Git sources

When a pool needs authentication for missing source objects, choose how this
client supplies them with `--git-credentials=ask|lend|local` on `run`, `attach`,
`start`, or `console`. The command-line option overrides `git.credentials` in
your client configuration. Omission defaults to `ask`. For example:

```yaml
git:
  credentials: local
```

This machine-wide setting covers all servers and repositories used by this
client. Saving `lend` authorizes future eligible pulls without another question.
Choosing an option in a dialog does not save it.

- `ask` explains both choices before you confirm. Without interactive input it
  fails with the configuration path and a retry command when a choice is needed.
- `lend` sends your local Git helper's username/password or token through the
  control plane to the pool for the synchronous HTTPS pull. Discobox does not
  save it in server/pool resources, cache Git configuration, or credential stores.
  Your local helper may save it on success. Discobox's use ends with the request;
  the original credential keeps its provider scope and expiry and is not revoked
  or restricted to this repository. Trust the server and pool with it. If the
  pool pull cannot finish, the client reports its switch to local fetching.
- `local` authenticates with this machine's Git configuration, downloads objects
  and sends them to the pool, without lending the upstream credential. Objects
  remain cached and the initial download travels through this machine twice.

These choices govern consent, not the location of every fetch: public sources
and commits already cached need no question. A failed delivery leaves the box
awaiting source; use the reported attach command to retry. Noninteractive
operations suppress Git, askpass, credential-manager and SSH questions; configure
a noninteractive credential helper or an SSH key and known host first.

A discobox creating another discobox uses its proxy to fetch locally and cannot
lend credentials. Use `local` there. The older `run --lend-git-credentials`
spelling still works: true means `lend`, false means `ask`; it cannot be combined
with `--git-credentials`.
