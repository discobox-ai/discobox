# Provision Design

How a box is provisioned from its features
([ADR 26-10-16-423](../../docs/adr/26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
§§3–6). What a feature's `provision.yaml` may say, and the `provision.json`
document, are the root [`provisionspec`](../../pkg/provisionspec) contract; this
package is what is done with them.

## Steps

A feature's steps are the runner's typed kinds: `file` (with `merge: json`, an
RFC 7396 merge patch over the file the box already has, which is refused
rather than replaced when it is not a JSON object, so a provider's
`daemon.json` keeps what ours does not set; its `restart` names the units that
read the file only as they start), `systemd` (enable, mask, drop-in,
through `systemctl`), `group` (asked of `runuser`, then `groupadd`), `env` (a
`/etc/profile.d/zz-discobox-<feature>--<step>.sh` script, named to sort after
the box's own profile scripts so one that sets PATH outright does not drop it,
which also puts its `path` ahead of `/usr/local/bin`'s shims; it reaches login
shells only, never an exec that gets the manifest's env; a feature's scripts
no step selecting the box still writes are removed with their records),
`download` (fetched through the environment's proxy, given up after a minute
without a byte, and moved into place only once its sha256 matches; with
`unpack`, a gzipped tar or a zip, told apart by its first bytes, unpacked
beside its directory and swapped in whole, refusing an entry or link that would
land outside it, and a zip whose entries carry no Unix modes), `packages`
(`apt-get`, keeping a configuration file already in a package's way rather
than asking; a list per distribution, the box's own os-release `ID`'s, else the
first `ID_LIKE`'s that has one, and a box no list names fails the step), and `run` (whose `inputs` name the files and directories its script reads
beside itself, so the digest covers them), each selected by `os`, `arch` and os-release
`ID`/`ID_LIKE`. On a booted box (`Runner.Booted`, systemd running: a box's
boot and a snapshot's, never an image's build) an enabled unit is started, and
a unit a step places a drop-in for, or a file names in `restart`, is restarted
if it is running (`systemctl try-restart`), once, after the feature's steps,
all in one call so systemd orders them (`Step.Restarts`): it read them as it started, and the
box may have started it before the feature was provisioned — containerd and a
socket-activated dockerd are up before the `docker` feature lays their drop-ins
— so it would otherwise run without them until the next boot. Each step is keyed by a digest of its definition and every
file it names, recorded under `/opt/discobox/provision/steps/` on the root
filesystem, never a declared volume; a step whose record matches is skipped
unless its `check` exits non-zero (drift), so a second run changes nothing. A
box that is not Linux of the Debian family is refused by name before any step
runs, unless it has no features to provision.

## Who runs it

| Caller | Entry | Runs |
|---|---|---|
| The agent, every boot | `Boot(…, wireOnly=false)` → `Runner.Provision` | Everything, with `Background`: the server answers meanwhile |
| `boot.Wire` / `boot.Provision` | `Boot(…, wireOnly=true)` → `Runner.Wire` | Wiring alone, before the account exists |
| `discobox-sandbox-agent snapshot` | `Snapshot` → `Runner.Provision` with `Snapshot` | Every step blocking, the plan's features off, and what a feature keeps out of a snapshot `Deferred`: a box provisioned as a snapshot is taken ([Snapshots](#snapshots)) |
| `discobox-provision apply`, on a pool | `Pool` → `Runner.Provision` | Every feature of the pool's bundles, every step blocking, nothing off and no placeholders: nothing runs on a pool but what its bundles lay ([ADR 26-10-24-953](../../docs/adr/26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md) §2) |

At boot the trees are the box's pins' bundles, in the pins' order
(`bundlestore.Trees`, handed to `BootPlan`): a sandbox's sandbox bundle and
then its harness's, or a pool's pool bundle. Each is read at its stable link
in the content store, `agentlayer.BundleTree(name)`, pointed at the pin's
tree before the run starts, so the paths a step or a link names stay the
same across upgrades and follow the trees; a step's record keys its
feature's directory too, so one applied from another path is applied
again. Every feature's directory moved when the trees came to be linked by
name, so the first start of a box made before on a new agent runs every step
once more, which they are idempotent for (ADR 26-10-24-953, Consequences). Nothing says which
tree is which ([ADR 26-10-24-953](../../docs/adr/26-10-24-953-every-box-is-provisioned-from-an-ordered-list-of-bundles-and-a-pool-is-a-box.md)
§1): order is the tie-break of what runs first, a later bundle's blocking
features after an earlier one's, and what blocks is each feature's own
`async`. A step that lays a binary the build made links it from the tree
rather than copying it, so a new agent is a new file and no step applied
again. A box with no trees has no features: `provision.json` lists none, and
its tools are its image's. Opt-outs are the bootstrap's `features`
(`sandbox.json`), which the client read from the repository's
`.discobox/settings.json`.

Core's steps never lay the agent's own units: they boot a box before any
feature is provisioned, so they are root `agentlayer`'s, placed by the pool
with the agent.

## A run

```mermaid
flowchart TD
    plan["plan: order by requires,<br/>probe adopt, decide off"] -->|refused| fail["readiness failed"]
    plan --> wire["wiring of every feature on:<br/>group and env steps"]
    wire --> ph["placeholders for features<br/>left to the background"]
    ph --> block["blocking steps in requires order:<br/>every feature not async, what they require"]
    block --> ready["readiness decided<br/>(Tracker.AwaitReady)"]
    ready --> bg["the rest, in parallel,<br/>each after what it requires"]
    bg --> done["provision.json: ready or failed"]
```

- **Outcome.** A feature turned off, and required by nothing, is `off`: no
  step runs and its env scripts go, but nothing is uninstalled. One requiring
  an `off` feature is off too. A feature that is not `async`, or that one
  requires, is refused when turned off, naming both — unless its `adopt`
  check passes.
  Core (`provisionspec.Core`) is never off: the server refuses it in a
  create's `features`, and a run refuses it outright.
- **Adopted.** `adopt` is checked by the agent, not a script: each command on
  `SystemPath`, run by every user, never in a home directory, never the
  agent's own personality or a placeholder; the version floor; the writable
  paths. Adopted skips the `install: true` steps and applies the rest. A
  feature with a record of an install step is ours and is never adopted.
- **How.** Ready is `adopted`, `installed` when this run applied a step, else
  `snapshot`.
- **Blocking.** Every feature that is not `async` — core, a harness's
  features; the platform's optional features are `async` — and *whole*:
  everything a blocking feature requires, every step, its `async` marks
  overridden, since a blocking feature needs all of what it requires. In a
  blocking feature a step marked `async` defers itself and every step after
  it, so steps still run in the order written. Without `Background` every
  step blocks.
- **Failure.** A failed step is not recorded, fails its feature and those
  requiring it, and nothing else. A failure before readiness fails it, and
  its error names each failed feature's log.
- **Locks.** `packages` steps and `run` steps marked `apt` take one lock; a
  `file` step with `merge: json` takes another across its read and its write,
  so features merging into one file in parallel keep each other's keys.

Npm install scripts use a private temporary cache and remove only that cache.
They run while a box provisions, at its boot or as a snapshot is taken, when the user's npm cache can be
a mounted, pool-shared volume; provisioning must not clear or remove it.

## Snapshots

A snapshot is a box provisioned for no sandbox, and kept so a box of the same
plan starts from it (ADR 26-10-16-423 §8). The run that takes one
(`Options.Snapshot`) leaves out every step its feature keeps out of a
snapshot — the step's `snapshot: false`, or the feature's, which is every
step of it — as `Deferred`, with every step written after it and every
feature requiring its feature, so nothing runs before what it follows. It
records none of them, so each box made from the snapshot applies exactly
those at its own boot, in order, and skips the rest on their records.
Mark what must be each box's own: anything keyed to the machine, or
provisioned fresh on purpose. `snapshot` is not part of a step's digest
(`StepDigest`): it says when the step runs, not what it does.

## Its state

`Tracker` holds the document, replaces `provision.json` whole on each change,
and is what the agent's readiness gates (`terminal`'s `AwaitProvision`, the
declared services) and status report read. Each feature's steps write to
`/var/log/discobox/provision/<feature>.log`.

## Placeholders

A feature left to the background has each of its `commands` written to
`PlaceholderDir`, last on every PATH (the image's PATH, and a profile script
the run writes when it writes one). A placeholder execs `discobox-provision
run FEATURE COMMAND`, which says on stderr that it waits, waits on
`provision.json`, then finds the real command past the placeholders and execs
it. A feature's placeholders go when it is ready, and every boot replaces the
set. `discobox-provision wait FEATURE...` is the same wait for anything else.
