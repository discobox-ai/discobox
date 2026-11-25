# Concepts

## Boxes and sources

A discobox, or box, is the sandbox one harness runs in: its own writable
disk, terminals, services and copy of your source. You can give it
passwordless sudo and nested Docker without giving it your host checkout. A
box can be short-lived: one per task, archived once its work is pushed. Or it
can be long-lived: a named agent with its own files, logins and credentials,
which you stop, start and talk to for as long as you need it. A box's name is
a display label, and its ID is unambiguous. Names and tags help you find a box
among parallel tasks.

`discobox run SOURCE [PROMPT...]` creates a box. A source can be a local
repository, a repository URL or `OWNER/REPO`. A local source starts at the
checked-out commit. Uncommitted changes come along only with
`--include-dirty`, and they arrive in the box still uncommitted. `-i` adds
more sources. Given a directory that is not a Git repository, run asks at a
terminal whether to copy it. Without a terminal, run copies it without asking.

The box fetches its source through a read-only origin. Your host's files
stay separate. `discobox push BOX` delivers your local commits into an
existing box. It does not bring the box's changes back. Commit and push the
agent's result to a Git host, then review and merge it in your normal workflow.

## Harnesses

A harness packages an agent CLI and the instructions for launching it.
The built-in harnesses include Claude Code, Codex, OpenCode, GitHub Copilot,
omp, pi and a shell. `discobox configure` opens the harnesses screen, where you
review settings and logins. `discobox run -H HARNESS` selects one for a task.

Discobox applies a harness's saved configuration when it provisions a box.
Changes you make to a harness in one box are saved back as shared harness
settings only after you review them. The console's `H` action opens that
review. Inspect its changes before you save.

## Skills

A skill is a folder of instructions a harness can follow: how your team
triages an issue, fixes a bug, or opens a pull request. Each subdirectory
holding a `SKILL.md` is one skill.

- `discobox run --skills DIR` copies the skills in `DIR` into the box.
- `discobox run --skills URL[@REF][//PATH]` checks a Git repository out in the
  box and links its skills rather than copying them, so an improvement to a
  skill can be committed and pushed back for the next box.
- `--user-skills` brings your own `~/.claude/skills` and `~/.agents/skills`.
- A repository can declare skills under `.discobox/skills`, and
  `discobox admin skills` manages skills repositories every new box in the
  project gets. `--no-project-skills` leaves those out of one box.

## Boxes that lead other boxes

A box created with `--discobox-api` can create boxes of its own and lead
them: run commands and copy files in them, read and type into their
terminals, wait until they finish or ask a question, start and stop them, and
archive them when their work is done. It acts only on the boxes it created
or was allowed to adopt. A lead box given a credential to delegate
(`--delegate`) can bind it to the boxes it creates and answer their access
requests itself, within the routes it was delegated.

This is how a software factory runs on Discobox. Skills define each step of
your process: how to pick up a task, do it, and record the result. The state
of the work lives somewhere every box can read and write, such as GitHub
issues, labels and pull requests. A long-lived lead box watches that state and
starts a box with the right skill for whichever step is due. Each box moves
one piece of work one step forward and is done. A person still makes the
decisions and merges.

## Providers and pools

The server is the control plane. A provider supplies the machines or
containers that boxes run on. A pool uses a provider to run boxes, and shares
downloads and build caches among them. The CLI can connect to a local or a
remote server, so the same command on your machine can run a box on a
different host. A project groups its
boxes, harnesses, pools and secrets.

Discobox has built-in providers, so on your desktop boxes run on your own
machine with nothing else to set up. Other providers run boxes in the cloud,
on third-party sandbox and infrastructure platforms.
Whichever provider runs a pool, harnesses, sources, secrets, policy and the
pool proxy work the same way. Choose the pool for one box with
`discobox run --pool POOL`.

`discobox admin provider catalog` lists the provider types the connected
server offers. Each provider type's own flags for create and update come from
that server's catalog at run time. They are not compiled into the CLI, and
they can differ between servers and versions. Read both the catalog and the
matching help before you configure a provider:

```sh
discobox admin provider catalog -o json
discobox admin provider create --help=docker
discobox admin provider update --help=docker
```

The [CLI reference](cli.md) lists every visible static command and flag,
including the generic `--type`, `--config` and `--secret` options. It does not
list the provider fields a server supplies at run time. For their names,
required values and descriptions, run `--help=TYPE` against the server you will
change. You supply secrets separately from provider config. Never put a token
in a documentation snapshot or a catalog fixture.

### VM providers: boxd, archil and hcs

Each VM driver the server links is a provider type of the driver's own name.
Its own settings are under `driver`, as `--driver-*` flags, and its key is the
secret `driver.apiKey`. The catalog reads them from the driver, so
`--help=boxd` on your server is the complete list.

| Type | Driver fields | Pools |
| --- | --- | --- |
| `boxd` | `driver.apiKey` (secret, required), `driver.region` (required), `driver.addr` and `driver.tokenUrl` (advanced endpoints) | Linux |
| `archil` | `driver.apiKey` (secret, required), `driver.region`, `driver.baseUrl` (advanced endpoint) | Linux. The pool's machine never holds the key. It holds a stand-in, which the pool's proxy swaps for the key only on its calls to Archil's API |
| `hcs` (Windows) | none | refused until Windows pools |

`boxd` and `archil` also take `controlPlaneUrl`, `base` and the pool policy.
An `archil` provider with no `base` makes its machines from the sandbox base
image of an earlier Discobox release,
`ghcr.io/discobox-ai/discobox-sandbox-base`, pinned by digest. It does not use
Archil's default image, which has no systemd. A `base` you set replaces it. A
`boxd` provider with no `base` uses boxd's default image.

Every `driver` field is fixed while the provider has pools. The driver sends
your key to the endpoints its endpoint fields name, so the server checks them.
Each must be a public host over https, or a public `HOST:PORT`. A write that
sets or changes one must carry `driver.apiKey` in the same request. To allow a
private endpoint, the server's operator lists it in `providerEndpoints`
(`DISCOBOX_PROVIDER_ENDPOINTS`).

Creating a provider checks only its config. `discobox admin provider check`
asks the server whether it can reach what the provider's pools need. It makes
nothing, and calls the provider's service with the key. It answers one of:

- `ok`.
- `refused`: the service answered and refused, for example a key it does not
  accept or a region not enabled. Fix the provider's config.
- `unreachable`: nothing answered. Fix the server's network or proxy.

A provider's key never goes on a pool's machine. Where the driver allows it,
as boxd does, a pool's machine authenticates as itself. Otherwise it uses a
key the driver issues to the pool. Discobox refuses a pool on a driver that
does neither. [Running boxes on boxd](boxd.md) sets up a `boxd` provider step
by step. It also says what the key and the pool machines can reach, and how
to rotate or remove the key.

On upgrade, a `vm` provider on boxd becomes a `boxd` provider with the same
ID, pools and key (see [Running boxes on boxd](boxd.md#upgrading-a-vm-provider)).
A `vm` provider on any other driver, such as vz, is disabled. Delete it and
its pools yourself. Those pools never ran.

When the server is behind an egress proxy, set with `HTTPS_PROXY` or a SOCKS5
`ALL_PROXY`, it reaches a VM provider's endpoints through the proxy by name,
and the proxy resolves them. The proxy decides what the server can reach. If
the proxy can reach your internal network, have it refuse private addresses,
since a project member can name a provider endpoint.

### DNS on machine providers

Discobox tries to make `169.254.53.53`, its local DNS stub, the system resolver
inside a machine box. The stub carries lookups through the pool. If the
provider locks `/etc/resolv.conf` or keeps replacing it, Discobox leaves the
provider's resolver in place and logs that it could not replace it. Proxy
traffic still works then, but programs that resolve names themselves use the
provider's DNS. The stub stays available to programs that query its address
directly.

## Lifecycle and storage

The console shows each box's state: running, starting, stopped, archived or
error. Stopping a box frees its runtime resources and keeps its persistent
workspace. Starting it resumes or boots it. A box that was frozen keeps its
terminals, and one that was shut down relaunches them. Leaving an attached
window does not stop the box. The box and its sessions keep running.

An idle box stops on its own. Terminal output, connections and keepalive
leases count as activity. A server running in the background is not enough,
on its own, to keep a box awake. The pool sets the idle timeout.
A box that stopped itself says why in `discobox ls`: `stopped (idle)`,
`paused (asleep)` after an agent put it to sleep, or `stopped (shell exited)`;
`discobox events` says `why=idle`, `why=sleep` or `why=exit`. A plain
`stopped` was stopped by a person, or went down some other way.

Archiving hides a box and starts its retention period. While the box is
retained, unarchiving recovers it. Purging destroys its data for good. Push
useful work to a remote before you discard a box. Shared caches make boxes
faster. They are not trusted copies or backups. Never put a secret in a cache.

### Disk use

A box's own disk is its workspace and home, its container's writable layer
(whatever it installed outside its workspace, such as system packages), and
the traffic the pool's proxy recorded for it. A fresh box holds little more
than its source. One that has installed its dependencies and built its
project commonly holds several GB, since its packages, build output and
language toolchains are its own. Stopping a box keeps all of it. Archiving
removes the container, and with it the writable layer, but keeps the
workspace, home, config and secrets until the box is purged.

What boxes share is counted once per pool, not once per box: the images every
box runs from (the sandbox base and each harness, a few GB together), the
pool's package and build caches, its build store, its registry of images boxes
pushed, and its proxy's cache of downloads. These grow with use.

`discobox admin pool resources` shows the real figures: each box's disk, what
the pool shares, and what is not attributed to either.
`discobox admin box resources BOX_ID` shows one box. See the
[CLI reference](cli.md#discobox-admin-pool-resources).

#### Where the disk lives

Each built-in pool is a virtual machine with its own disk files on your
machine:

| OS | Files, per pool |
| --- | --- |
| macOS | `data.raw` and `cache.raw` in `~/Library/Application Support/discobox/vz/pools/POOL_ID/` |
| Linux | `data.raw` and `cache.raw` in `~/.local/share/discobox/libkrun/POOL_ID/`, or `local-vm/POOL_ID/` there on a machine that ran a release from before the provider was named libkrun |
| Windows | `storage.vhdx` in `%LOCALAPPDATA%\discobox\wslc\POOL_ID\` |

On macOS and Linux, `$XDG_DATA_HOME` replaces `~/Library/Application Support`
or `~/.local/share` when it is set, and a provider configured with its own
state or storage directory keeps its pools' disks there instead.

Each disk has a size limit: 100 GiB of data and 50 GiB of cache on macOS and
Linux, and 100 GiB on Windows. The files are sparse, so a file takes only the
space the pool has written. The files grow as boxes and images are added. They
do not shrink when boxes or images are removed. The pool frees the space
inside, where new boxes reuse it, but the files on your disk stay at their
largest size. A Linux pool that runs on Docker, because the host cannot run
libkrun, keeps its boxes, images and caches in Docker's own storage instead.

#### Reclaim space

1. Push or collect the work you want to keep from each box first.
2. `discobox rm BOX` archives a box. Its data stays until the archive
   retention period runs out. That period is the project's, set with
   `discobox admin project update --archive-retention`, or else the server's,
   which is 24 hours unless the server is configured otherwise.
3. `discobox admin box purge BOX_ID` destroys a box's data now, archived or
   not, and returns once it is gone. In the console, `P` purges the selected
   archived box, after asking.
4. `discobox admin pool clear-cache POOL` stops the pool's running boxes and
   empties its caches, its build cache and its registry. An image a box pushed
   to the registry is gone until the box pushes it again. The command also
   removes the images no box uses, except the sandbox base image, the newest
   image of each repository a box still uses, and any that arrived in the last
   hour. The boxes start again the next time they are used.
5. On a pool with disk files, that frees space inside the pool but not on your
   disk. To shrink the files, replace the pool with a new one on the same
   provider, then delete the old one. A pool that still has boxes, archived
   ones included, cannot be deleted, so purge them all first:

   ```sh
   discobox admin pool create NEW_POOL --provider PROVIDER
   discobox admin pool set-default NEW_POOL
   discobox admin pool rm OLD_POOL
   ```

   PROVIDER is the built-in provider, named `macOS`, `libkrun` or `Windows`
   (`discobox admin provider ls` lists them). The old pool's files are
   removed shortly after it is deleted.

## Credentials

Managed secrets stay outside the sandbox. A box receives a placeholder,
called a sentinel. The pool proxy swaps in the real credential only on allowed
requests. A binding sets who may use a secret, which routes it allows, and how
long it lasts. A binding belongs to a project, a harness or a box. A raw
credential is a different grant: when you allow one, the box can read it.

An agent can request access it lacks. Review the destination, methods,
paths, lifetime, and the routes behind any named role. A request may ask for
use, delegation to children, or both. Decide each part. Approving use does not
approve delegation. The console marks boxes with pending
requests; open the request action to approve or deny it. Do not approve a
role just because its name sounds appropriate.

Every request a box makes leaves through its pool's proxy: the box's network
has no other route out. The proxy applies egress policy to each outbound
request and audits it. An egress policy can restrict a box to allow lists of
hosts, wildcards, IP addresses or CIDR ranges. You set it on the project, the
pool, the harness or one box. Isolation and credential routing do not make
code an agent wrote trustworthy. Review code and results before you merge them
or run them on your machine.

## Services, ports and tools

Repositories can declare services under `.discobox/services`. They start at
boot, and their output appears in service tabs. A service that exits stays
exited. Discobox finds listening ports, or reads them from a declaration, and
forwards them while the client is connected. The console shows where to reach
them.

Tools under `.discobox/tools` open editors or other programs for a box.
`discobox tools ls` lists the available tools. A repository's tools run
inside the box. Only tools the user owns may ask to run on the user's host.

## Private Git sources

When a pool needs credentials to fetch source objects it lacks,
`--git-credentials=ask|lend|local` on `run`, `attach`, `start` or `console`
chooses how this client supplies them. The flag overrides `git.credentials` in
your client configuration. The default is `ask`. For example:

```yaml
git:
  credentials: local
```

This setting covers every server and repository this client uses. With
`lend` saved, later eligible pulls go ahead without another question. Choosing
an option in a dialog does not save it.

- `ask` explains both choices before you confirm. When a choice is needed and
  there is no interactive input, it fails with the configuration path and a
  command to retry.
- `lend` sends your local Git helper's username and password, or token, through
  the control plane to the pool, for the synchronous HTTPS pull. Discobox does
  not save it in server or pool resources, in cached Git configuration or in
  credential stores. Your local helper may save it when the pull succeeds.
  Discobox stops using it when the request ends. The credential keeps its
  provider's scope and expiry, and Discobox neither revokes it nor limits it to
  this repository. Lend it only to a server and pool you trust. If the pool
  cannot finish the pull, the client says so and fetches locally.
- `local` authenticates with this machine's Git configuration, downloads the
  objects and sends them to the pool, without lending the upstream credential.
  The objects stay cached, and the first download passes through this machine
  twice.

These choices decide consent, not where every fetch happens. Public sources
and commits already cached need no question. A failed delivery leaves the box
waiting for its source. To retry, run the attach command the client prints. A
noninteractive operation suppresses Git, askpass, credential-manager and SSH
prompts. Configure a noninteractive credential helper, or an SSH key and known
host, first.

A box that creates another box fetches locally through its proxy, and cannot
lend credentials. Use `local` there. The older spelling
`run --lend-git-credentials` still works. True means `lend`, and false means
`ask`. It cannot be combined with `--git-credentials`.
