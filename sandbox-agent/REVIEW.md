# Sandbox Agent Review Notes

## The DNS stub's firewall

- **A new way of putting a sandbox on a shared network keeps its interface
  under `dnsstub.pooledInterfaces`.** The stub drops what arrives for its
  address only on `eth+`, the names Docker gives a container sandbox, and
  `en+`, the names systemd gives a provider machine's NICs; anything else is
  let through, because the nested Docker's bridges must reach it. A sandbox
  attached under another name — an endpoint given a
  `com.docker.network.container_iface_prefix`, an interface an image unit
  renames, a provider whose guests keep a kernel name outside both — would
  answer its pool neighbours under its own certificate (ADR 26-10-15-760,
  ADR 26-10-16-423 §9).

## Provisioning

- **`kvm`'s GID is pinned in the sandbox base image; keep it there.** A
  sandbox is privileged and sees the host's `/dev/kvm` with the host's numeric
  owner, so `image.json`'s `kvm` in `additionalGroups` grants access only where
  the image's GID matches the host's. Package installs allocate system GIDs
  in their own order (on Ubuntu 24.04 the same image's `kvm` came out 995), so
  the base image creates it at 992 before any package runs: the GID earlier
  Debian 13 images gave it, and the one the Pop!_OS host these images were
  measured on uses. A change to the base image's distribution or packages, or
  a new group that guards a host device, has to check the GID the same way,
  not assume it.

- **A feature that differs on Ubuntu is checked on Ubuntu.** The sandbox base
  image is Debian 13, so no box of a Docker pool provisions a feature's
  `ubuntu:` list or a step selected by `when: {distro: [ubuntu]}` — the paths
  exe.dev's and boxd's Ubuntu 24.04 machines take. `task
  build:ubuntu-sandbox-base` builds the base on Ubuntu 24.04; point a pool at
  it (`DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE`) and see those steps apply in a box
  of it.

- **A file a running daemon reads only as it starts names it in `restart`.**
  A booted box may have started the daemon before its feature was provisioned
  — containerd and a socket-activated dockerd were up before the `docker`
  feature laid their proxy and runc drop-ins, and nested containers had no
  way out until the next boot. A drop-in restarts its own unit; a plain
  `file` step (`daemon.json`) has to say which units read it.

- **A step that must be each box's own says `snapshot: false`.** A snapshot
  is shared by every box of its plan (ADR 26-10-16-423 §8), so a step that
  writes something keyed to one machine — a host key, an ID, a generated
  secret — or that must run fresh on every box is kept out of it, on the step
  or its whole feature; everything after it, and every feature requiring it,
  is left out with it. The snapshot command also empties `machine-id`,
  removes SSH host keys, and untrusts the build's proxy CA itself
  (`cleanMachine`); anything else a package generates per machine is the
  feature's to keep out.

- **A snapshot provisions with none of a sandbox's mounts.** No primary
  volume, no pool cache, no declared volume is bound in a build, as none was
  in an image build: a step that writes under a declared `cache` path, which
  a plain bind hides at each box's boot, ships its content aside instead (as
  `nix` seeds its store), and one under a `data` path relies on the wiring's
  overlay. A step that needs a box's mounts in place says `snapshot: false`.

- **A snapshot is anonymous.** `discobox-sandbox-agent snapshot` runs from an
  `agentlayer.SnapshotPlan`, whose `Validate` refuses a credentials endpoint,
  secrets and sources. Nothing it reads may come from a sandbox's bootstrap,
  and nothing it writes outside the provisioned features may land on the root
  filesystem: its intake's record is `agentlayer.SnapshotStateFile`, under the
  directory its taker removes.

- **A `merge: json` source that stops setting a member sets it to `null`
  instead of dropping it.** The merge keeps every member the file there has
  and the source does not name, which is what keeps a provider's settings; it
  cannot tell those from one an earlier version of the source wrote, so a
  member simply removed stays on every box provisioned before, for good. The
  box's own value of a member the source overwrote is not kept either.

- **An env step's `path` goes ahead of `/usr/local/bin`.** Its script sorts
  after the box's own profile scripts (`zz-discobox-…`), so every directory
  it lists shadows the `docker` and `nix` shims there (ADR 0044 §8, ADR 0075
  §4). A feature that moves a toolchain onto an env step must not list a
  directory holding a `docker` or `nix` of its own; and since the script
  reaches login shells only, a variable a non-login exec needs still belongs
  in the manifest's env.

- **A runtime integration reads `provision.json`, never the box.** Whether a
  feature is there is decided once, by provisioning (ADR 26-10-16-423 §4); a
  boot step or service that wires one checks `provisionspec.FeatureOff` (or
  the agent's `Tracker`), and treats a feature the box does not report as its
  image's. Probing for a binary instead disagrees with an `off` feature still
  on disk.
- **Every string in `provision.json` is the box's.** Feature names, steps,
  errors and logs come from bundle trees and scripts; a reader outside the box
  escapes them as it does all text a discobox wrote (`cli/REVIEW.md`).
- **Never wait on provisioning from the agent's start.** The agent serves
  first; only the readiness gates wait (`Tracker.AwaitReady`).
- **Services wait for their own provisioning dependencies.** An empty
  `features` list waits for the full run, including async features; a stated
  list waits only for those features. Gate all launches in `services.Manager`,
  including API starts and restarts. Boot launches wait independently, so a
  default service cannot hold up one whose stated features are ready. Do not
  put the harness's `AwaitReady` gate in front of service discovery again —
  nor in front of anything else declared that boot starts: monitors wait for
  the full run too (`Tracker.AwaitFeatures(ctx, nil)`), because the image's
  monitors and the `gh` and `jq` they run on come from the async `cli`
  feature. The mistake has been made twice.
- **Wait for provisioning outside service lifecycle locks.** A restart can
  wait minutes under a live service; holding its lock prevents Stop and other
  requests from returning. Recheck live state under the lock before launching.
- **Discover provisioned image services after provisioning too.** Early
  discovery lets repository services launch independently, but a feature can
  install an image declaration later this boot. Revisit newly installed image
  declarations once the run ends, even on failure, without autostarting new
  repository declarations or retrying services already attempted. Recheck a
  builtin's owner after its wait: discovery can precede the plan marking its
  feature off, and that declaration must then stay hidden and never launch.
- **Every terminal launch waits for provisioning, not only the primary's,
  and a terminal that needs no launch does not.** `terminal.Service.awaitProvisioned` is called by
  `Create`, the primary's launch, and `Revive` — an attach by a terminal's own
  id revives it without the primary's launch. A new launch path calls it too,
  on the caller's context before the detached single-flight launch, or a
  harness starts after a reboot before what it requires is back. A terminal
  still running, or a launch already in flight (which the caller joins), skips
  the wait: an agent restart provisions again under a live harness, and an
  attach to it must not wait on that run or fail with it.
- **A step that reads what it then writes takes a lock.** Background
  features run in parallel, so a read-modify-write of a file two features
  touch — a `merge: json` file step into `daemon.json` — loses one's change
  unless it holds a lock across both (`Runner.merge`, as `Runner.apt` for apt
  and dpkg). Both steps are then recorded as applied, and the loss is never
  repaired.
- **A blocking feature needs all of what it requires.** Everything a
  feature that is not `async` requires blocks readiness whole, its `async`
  marks overridden (`featurePlan.whole`); an `async` step in a blocking
  feature defers itself and every step after it. Whether a feature blocks is
  its own `async`, never which bundle it came in: a platform feature that
  must not hold up readiness says `async: true`. Readiness reported while a requirement is in the background runs
  core's steps without it.
- **The content store trusts a digest, never a response.** `bundlestore`
  checks a manifest's bytes and a blob's against the digest that named them
  before it parses or extracts either; a layer counts as present only with
  its `.complete` marker, written after its directory is whole. Never read a
  pin from anything but `agentlayer.PinsFile`, which the pool places: what
  the box may fetch is the pool's record, and a pin the box chose would
  only be refused there.
- **The agent bundle is one layer, the agent alone.** Whoever makes a box
  places it from the `agent` stage's one layer (`agentlayer.ExtractAgent`,
  ADR 26-10-24-953 §2); a second `COPY` there stops every box booting. The
  platform carries no agent: core links the placed one
  (`/opt/discobox/current`), never a copy in a tree.
- **Every `COPY` into the `platform` stage is `--link`.** Without it each
  layer carries `/features/` as the stage last left it, so an edit to one
  feature moved every layer after it, and every box fetched them all again: a
  comment in `go`'s provision.yaml moved eleven layers. A layer added there
  without `--link` brings that back silently.
- **A delivered box's first boot has no trees when wiring runs.**
  `discobox-wire.service` and the account step run before the agent fetches
  its bundles, so a feature's groups do not exist for the account to join and
  its env is not wired then. What a feature makes before the account must be
  made again once the store is filled and before readiness
  (`boot.JoinFeatureGroups`): a first cut left the account without `brew`
  until the box's next boot.
- **A unit the intake starts that a feature lays must be started by that
  feature.** The intake skips a `proxyUnits` unit systemd has no file for,
  because a delivered box's first delivery lands before its features and
  failing it would hold the fetch that lays them. So on that boot nothing
  but the feature starts it: an `enable` entry in its `systemd` step (which
  runs `--now` on a running box), as the BuildKit bridge has, or a unit that
  pulls it up, as `docker.service`'s `Upholds=` does the nested-Docker
  bridge. A unit laid with neither never starts on that boot.
- **A step that works only before the box boots fails on every delivered
  box.** Those provision at first boot, with the pool's mounts in place
  (`/nix` is the shared cache). `install-nix.sh` installs into a private
  mount namespace for this reason; a step that refuses a mount, or writes
  where a volume will be bound, needs the same.
- **A box whose store holds its pins fetches nothing.** `state.json` maps
  each kept pin to its tree, and `Apply` takes that tree without asking the
  asset host; a boot that needs the network for what it already holds fails
  readiness whenever the pool's asset route is down.

## Run identity

Getting "which user does this run as" wrong is the most repeated mistake in this
module. It fails quietly — the process starts, and only some capability is
missing — so it survives review easily. Rules, and why each exists:

- **Resolve through [`runuser`](runuser/DESIGN.md), never by hand.** One call:
  `runuser.Resolve(layers, need)`, with the image, manifest and request layers
  and the fields the caller needs. Precedence and completion belong to it and
  to `sandboxuser.Merge`. The bootstrap's `user` (boot's `Wire` and
  `Provision`), `DISCOBOX_USER_*` (boot's `manifestUser`, in the export
  container) and `config.ExecDefaults` (the server's `execDefaultUser`) are read once
  each, as the manifest layer. Treating either as the resolved user is a second
  construction of the same identity, and the two always drift. That drift is
  exactly how terminals came to run without the sandbox's supplementary groups
  while plain execs kept them.
- **Outside boot, ask the exec manager.** `execs.Manager.ResolveUser(req)`
  supplies the three layers and does no merging of its own. `terminal`, the
  server and `ports` all go through it. Do not assemble layers yourself, and do
  not rebuild the default user.
- **Never invent an id.** No `uid = 0`, no `gid = uid`, no `uid = 1000` for a
  bare name. UIDs and GIDs are separate namespaces; `uid == gid` is a `useradd`
  default, not a rule. A missing id is read from the passwd entry, and a uid with
  no entry is an error.
- **Never fall back after a failed resolve.** Returning the error is the point;
  substituting a default reintroduces the guess.
- **Groups are all-or-nothing.** A request naming none inherits the sandbox's; a
  request naming any uses exactly those. Never union the two — merging makes the
  manifest a floor no caller can get under, so nothing can ever run with fewer
  groups than its sandbox.
- **A request chooses identity and membership independently.** Naming a user
  must not change groups, and naming groups must not change the user.
- **Group names resolve only in the sandbox.** `/etc/group` lives in the image.
  Code running on the pool host or in the control plane cannot resolve a name and
  must not guess a number for it; it leaves the value unset (`-1` for a chown)
  and lets the sandbox decide.
- **Provisioning asks `runuser` too.** A step that makes a group
  (`provision`) decides whether it is already there with
  `runuser.LookupGroupID`, not by reading `/etc/group` itself: one reader
  means its rules (a numeric entry is a gid) hold for every caller,
  `runuser.FixedDatabase()` fakes the database for every test, and two parsers
  of one file drift.
- **Boot never changes an account in place, and deletes before it resolves
  the group.** An account holding the sandbox's uid or name that is not
  exactly the sandbox's is deleted and the account made fresh (ADR
  26-10-16-423 §14); a `usermod --login`, `--uid`, `--gid` or `--home` on it
  is the rename that failed on a logged-in provider user (PR #180). The
  deletion comes before `ensureGroup`: `userdel` takes the account's own group
  with it, so a group resolved first can be gone by `useradd`.
- **A group the image never created is skipped, not fatal.** `runuser.Groups`
  drops it from the credential, the same way boot's `ensureAdditionalGroups`
  skips it, so the two cannot disagree about the same image. A harness Dockerfile
  that forgot to install a package must not break every process in the sandbox.

Decision records: [ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
[ADR 0033](../docs/adr/0033-user-resolution-is-one-layered-resolver-with-declared-gaps.md).

## What travels with a discobox

- **Never resolve a harness, secret or config by an ID the agent stored in
  its own state; resolve the sandbox's own.** `/var/lib/discobox` — the
  agent's database, its exec records with their `harnessId` among them —
  travels with an export (ADR 0129), and a config ID is local to the server
  that minted it (ADR 0123). The control plane resolves what its manifest
  names by name on import, but nothing rewrites what the box wrote down, so
  an ID read back from a record names the source server's config. Revive did
  exactly that, and a transferred discobox could not be attached (#356). A stored ID is
  what something was created for, which is history; what runs now is the
  bootstrap's.

## Testing identity

- Use `runuser.FixedDatabase()` with `t.Cleanup`. Never resolve against the
  machine's real accounts — `osuser.Current()` makes a test pass or fail on
  whoever runs it, and hides id assumptions behind whatever the host happens to
  use.
- Do not write fixtures where `uid == gid` — boot's passwd and group fakes
  included. That coincidence is what the rules must not rely on, so
  reproducing it hides the bug.

## Testing a resumable attach

- **A test that waits on `Delivery.Positions` keeps the attach drained the
  whole time** (`drainFrames` in `execs/shim_resume_test.go`), not only until
  the output it expects. `resume.Conn` takes an `Ack` in only inside
  `ReadFrame`, and the host may send the output an action caused before that
  action's `Ack`, so a test that stops reading at that output and then polls
  can wait forever for an `Ack` sitting unread (#417).

## Processes

- Credentials must set supplementary groups explicitly. `NoSetGroups` leaves the
  child holding the *agent's* groups — the agent is root — so a process dropped
  to the sandbox user silently inherits root's groups and none of its own.
- Identity resolution is per platform only behind `runuser`'s seam
  (`platform_linux.go`, `platform_other.go`), and both resolvers build
  everywhere so the one-account one is tested on Linux; keep identity logic out
  of `_unix.go` files, because darwin is unix and has no POSIX ids here. Only
  the credential and `SysProcAttr` construction are otherwise per platform
  (`execs/credential_linux.go`, `execs/credential_darwin.go`,
  `execs/process_unix.go`, `execs/process_windows.go`). Run
  `go tool task check:windows` before relying on that split.
- Off Linux, a request naming another user, a uid or a group set is refused
  with `sandboxuser.OneAccountError`, never dropped. Do not "tolerate" one by
  clearing the fields: that runs the exec as the one account while its record
  says it asked for someone else (ADR 0145 §5).
- A signal a platform cannot deliver as asked is mapped to its nearest real
  mechanism and recorded as an exec event. A new mapping goes in `procio`'s
  delivery tables with a `Reason`; one with no reason is a signal delivered as
  itself.

## Supervision

- **The supervisor's lifetime lock is `flock`, never `fcntl`.** A flock
  belongs to the open file description, so it travels to the shim with the
  descriptor and outlives the supervisor closing its own copy; a POSIX record
  lock belongs to the process and would be released the moment it did.
- **Only the shim takes the lock exclusively.** Every probe and wait in the
  agent takes it shared. An agent-side exclusive lock, however brief, reads
  as a live shim to a concurrent probe, and `Stop` then signals a pid that
  may already be someone else's.
- **The shim marks the lifetime descriptor close-on-exec before it starts
  anything** (`execs.HoldLifetime`). A command that inherited it would hold
  the lock past the shim's exit, and the exec would read as running for as
  long as anything it started did.
- **A stop ends the command's session, not its process group.** An
  interactive shell gives every job its own group; a group kill leaves a
  terminal's background jobs running after the exec reads stopped.
- **Never signal a session without asking whether it is still the
  command's** (`signalSession`, `endSession`). Every exec command leads a
  session, so "leads a session" does not identify one: a live leader must
  have the kernel identity the shim recorded (`Exec.ProcessIdentity`), and
  compared exactly. A wall-clock start within a tolerance is not an identity:
  a process that reused the number within it passes.
- **A lock that cannot be waited on is not a shim that ended.** Reading an
  error as "gone" demotes the exec to lost, and the terminal layer relaunches
  a lost terminal over the shim still running.
- **Write a runtime file only through `writeRuntime`/`writeRuntimeIfChanged`,
  and read it only through `readRuntime`/`readRuntimeFile`, never
  `os.ReadFile`.** The shim and the agent write it without a common lock;
  anything that writes it in place (`os.WriteFile`, `O_TRUNC`) can leave it
  torn, and a torn file silently loses the shim's exit (#203). On Windows a
  plain read racing the replace can fail for an instant too, which loses the
  exit the same way; `readRuntimeFile` waits it out. Anything staged in the
  runtime directory must not end in `.json`, or the watcher takes it for an
  exec.
- **A unit manager is required and complete.** Add behavior to `UnitManager`
  and to every implementation, including the test fakes; run the shared
  contract (`testUnitManagerContract`) against each.

## Where execs start

- **`DefaultWorkdir` is where execs start; `SourceRoot` is the repository.**
  A create may name a directory inside the primary source to start in (ADR
  26-10-15-664 §3), so the two differ. Anything that reads what the
  repository declares (services, tools, `.discobox/skills`) asks
  `SourceRoot`. Read from the start directory, a sandbox started in a
  subdirectory would lose its services.
- **A default workdir must exist when an exec starts.** systemd refuses a unit
  whose `WorkingDirectory` is missing, so a default that is not there fails
  every terminal and shell, not one exec. A start directory can be
  missing: never committed, gitignored, or removed since. It falls back to
  `SourceRoot` (`resolveWorkdir`). A new default must have a fallback that
  always exists.

## Reading the working tree at boot

- **Anything at boot that reads the sandbox's sources goes behind
  `agentRuntime.awaitSources`.** A sandbox's container can be running before
  its client has delivered its source, and so before its working tree exists
  (ADR 0001), so boot code that reads it
  is racing the delivery — and the race is silent, because an empty tree is
  indistinguishable from a repository that declares nothing. That is exactly how
  `.discobox/services` came to start on no sandbox at all: discovery ran ~3
  seconds early, found no directory, returned no error, logged nothing, and
  never looked again.
- **One gate, shared.** Take the field; do not call `sourcesready.Gate` a second
  time. Two constructions of the same wait is how one of them ends up gating
  nothing.
- **A one-shot at boot needs a reason it will not be re-run.** `EnsureStarted`
  runs once. Anything else with that shape has to be either gated or repeated —
  "it will be picked up later" is only true if something looks later.

## Materializing sources

- **Materialize once, then only assert the origin.** A pass that resets, cleans
  or checks out a source carrying `SourceMaterializedMarker` discards the
  sandbox's work. Anything a later pass must repair belongs in
  `ensureOrigin`, which touches only configuration.
- **Never reset a `.git` this agent did not move into place.** One with neither
  marker is the sandbox's own repository or a pool-made checkout; refusing it is
  correct, adopting it is data loss.
- **Nothing half-made at the target, nothing of the target's removed.** A clone
  that can be interrupted lands in the scratch directory; only a whole `.git`
  is renamed in, and a step that writes the target directly reintroduces #30.
  After the rename, write the tracked tree whole (`checkout --force`,
  `restore`) — never `clean`, which deletes what the target held before the
  clone, nested source targets included.
- **A target below home is made as root in a tree the user writes.** Boot's
  `wireSources` and `ensureHomeTarget` create a skills source's target under
  the user's home (ADR 26-11-01-692), and a link the user plants on the way
  can steer where root makes a directory. That crosses nothing only because
  boot gives the sandbox user passwordless sudo. A change that offers a box
  without it must make both refuse links race-free first (`os.Root`, or
  `openat` with `O_NOFOLLOW` per component). Why: security review of ADR
  26-11-01-692 found `ensureHomeTarget` checks each component before it makes
  the next, and `wireSources` follows links.
- **The token stays out of the checkout.** No `extraHeader`, no tokenized URL,
  no `-c` carrying it on a command line; git reads it through the helper only.
- **A path test for a route whose path carries a slug comes first** in
  `requiredRequestScope`: the slug is caller-chosen and can spell any test
  after it.

## Boot cost

Boot runs before anything in the sandbox is usable, so work here is latency the
user waits on every single start.

- **Boot never walks `$HOME`; it chowns only what it creates.** The shared
  pool cache and the source trees are mounted *under* home (`~/.cache`,
  `~/go/pkg/mod`, `~/.local/share/pnpm`, the source targets). They are
  unbounded -- ~1.3*10^6 inodes in one user's cache partition -- and already
  have their owners. A device check cannot fence them off: on a Docker pool
  the home volume and every cache volume are binds from one host filesystem
  and share a device, which is how a `st_dev`-bounded walk came to cost
  20-35s of every boot (#234). GNU `chown -R` is the same bug.
- **Decide ownership as a directory is made.** The directories boot invents
  on the way to a mountpoint -- `~/go/pkg` above `~/go/pkg/mod`, the path
  from home to a working root inside it -- go to the user through
  `mkdirAllOwned`, scoped to home so `/home` stays root's. That holds only
  while nothing makes them before boot: the pool sets no container
  `WorkingDir` for exactly this reason (`pool-agent/REVIEW.md`). `applyOwnership`
  speaks for each mountpoint, `seedHome` for the skeleton it copies into an
  empty home and nothing else. Anything new boot writes under home names its
  owner itself; nothing fixes it up later.
- **Removing a repair walk removes every repair it made**, not only the one
  it was written for. The walk this replaced also handed the user content an
  image shipped under home (through an overlayed home, or on a VM guest) and
  any path declared under `%HOME%` without a `uid`. Those now keep the image's
  ownership, which is the image's statement (see `mkdirAllOwned`); a path
  under home the user must write declares `%UID%`/`%GID%`.
- **A mount test of this uses a bind from the same filesystem**, never a
  tmpfs: a tmpfs has its own device, which is the case that was never the
  problem.
- **A data volume's layout is decided once and recorded, never re-judged.**
  `wireVolume` takes bind or overlay from `decideLayout`, which keeps the
  layout in `/.discobox/data/.layouts`; `useOverlay`'s look at the target is
  only for a volume with no record. Choosing from the target on every boot is
  what made a re-pinned box's home show up as `~/upper` and `~/work`. Never
  convert a volume between layouts by moving files: an upper is a diff over
  the image, not a tree. Never put the record in or beside a volume directory
  — a bind's directory is the target, and a nested volume's is inside its
  parent's. Write it the way `provision/records.go` writes a record (temp
  file synced, renamed, directory synced): boot refuses a record it cannot
  read rather than guess, so an empty one left by a crash fails every boot.
- **A test of guest-path code hands it slash paths**
  (`filepath.ToSlash(t.TempDir())`, joined with `path`). `boot` splits
  paths with `path`, which finds no directory in a `filepath.Join` result on
  the Windows CI runner, so a test built on `filepath` fails only there.
- **An overlay's upperdir adopts the target's identity before
  `applyOwnership`, never after.** overlayfs presents the upperdir's owner and
  mode as the merged root's, so `wireVolume` runs `adoptDirIdentity` (setgid
  included) before mounting. Then `applyOwnership` applies only what the path
  declares (ADR 0107 §3). Reverse the order and a declared uid/gid/mode is
  overwritten. Drop the adopt and every overlayed path shows up as root:root
  0755, which refuses writes at its top level only.
- **Ownership has one owner per path.** If the pool agent already owns a tree,
  boot must not assert it again. Both sides asserting produced two full walks of
  the same inodes, in opposite directions, on every start.

## Personalities

- **Every package init the agent links is paid by every `runc` and `docker`
  call.** Those are personalities of this binary (`cmd/discobox-sandbox-agent`),
  and a nested container start runs `runc` four times. A dependency that builds
  a table in `init` multiplies: `go-runewidth` before v0.0.31 cost 21ms of a
  ~40ms call. Check a new dependency with
  `GODEBUG=inittrace=1 /opt/discobox/bin/runc --version`.
- **A personality is a `personalities` entry and a link a feature lays**,
  core's `link-agent.sh` or docker's `link-personalities.sh`; a test holds the
  two together. A link with no entry starts the agent's server under that
  name.
- **A file the image installs is a step of the feature it belongs to**
  (`features/<name>/provision.yaml`), never a Dockerfile `COPY` or `RUN`: the
  Dockerfile copies feature directories in and provisions them. A new
  directory under `features` is copied and provisioned in its own layer, after
  what it requires, and copied into the `platform` bundle stage under
  `/features/` from the same source; `provision/platform_tree_test.go` replays
  the copies, holds the two lists equal, and fails otherwise. A script a step runs is committed executable, and is
  idempotent: a step is applied again whenever its digest changes, which on a
  booted box is with its volumes wired over what it installed — a script
  that rebuilds a tree a volume now holds (`/nix`, Homebrew's prefix) must
  refuse or touch only what it wrote.
- **A `run` step names in `inputs` every file of its feature its script
  reads**, or editing one changes nothing on a box that has the step
  recorded. A pinned release file of a third-party tool, with a known
  sha256, is a `download` step (`unpack` for an archive) rather than a
  script's own curl: the step checks the sha256 and puts the file or tree in
  place whole, where a script's fetch does neither unless it says so. An
  unpinned installer (go, uv, bun, rustup) stays a `run` script.
- **Anything recorded in an exec that names this binary uses its stable
  path** (`stableAgentPath` in `server`): `agentlayer.AgentPath` wherever
  the box runs its agent from the bundle store, never `os.Executable()`
  there. An exec record outlives the box's stop and is run again by its
  relaunch, and `os.Executable` resolves to the agent's layer under the
  store, which is swept once a new agent is pinned: a monitor's runner
  recorded that way exits 127 at every relaunch after an upgrade, and
  nothing says so. Only where there is no store — no layers to sweep — is
  the executable's own path the stable one.
- **A file shared between processes by renaming it into place goes through
  `sharedfile.Retry`**, never a retry of its own. On Windows a handle another
  process holds refuses an open, a rename or a remove for a moment, and only
  that — `ERROR_SHARING_VIOLATION`, `ERROR_ACCESS_DENIED` — is waited out; a
  loop retrying every error reports a full disk as a busy file. The exec
  runtime records and the monitors' queue each had their own once.
- **An in-box command that prints text a third party wrote escapes its
  control characters**, as `cli/REVIEW.md` requires of text a discobox wrote.
  A monitor's events carry issue titles, comments and fetched pages verbatim,
  and `discobox-monitor pop`, `read`, `status` and `logs` print them into a
  terminal the console relays: an unescaped OSC 52 reaches the user's
  clipboard, an OSC 7501 fakes the terminal's status. A monitor declaration
  is third-party text too — a repository's or a skills source's author's — so
  `list`, `show` and `enable`'s output escape its id, name, description,
  path and problem, and `discobox-monitor` escapes every error where it
  prints it, so a path a third party named cannot slip through an error made
  far from the CLI. `--json` output needs nothing;
  `encoding/json` escapes them.
- **A monitor's runner removes its enabled file only if the file still holds
  what that run acted on** (`finishIf`). `enable --update` rewrites the file
  under a running instance, so a run that started with the old options and
  ends with a final event must not remove the file the update wrote. The
  CLI's `enable --update` and `disable` record the user's intent and write
  without such a check.
- **A directory named `build` is ignored by the root `.gitignore`**; the
  `build` feature is kept by a negation there. A feature or test directory
  of that name elsewhere needs one too, or a clean checkout lacks it while
  the author's tree builds.

## Platform observation

- **Ports, resource counters and power-off go through their seam**
  (`ports.Scanner`, `resources.Sampler`, `autostop`'s `platformPowerOff`). A
  new `/proc`, cgroup or `systemctl` read outside the Linux implementation is
  a Linux-only answer that breaks on darwin without failing a Linux test.
- **Keep the tagged files to the system calls.** Parse what a platform's tool
  prints in an untagged file, so the parser is tested in every CI lane rather
  than only on a Mac.

## Idle stop

- **Every client connection this process serves must hold `autostop`** —
  exec attach, one-shot attach, `tcp/attach` and `udp/attach` today — for as
  long as the client is connected. A files API write holds it too; a files
  API read does not, since a hold's release is activity and reading is never
  activity (below): an editor listing a discobox must not keep it up. The shims' attacher counts are not enough: a tunnel has
  no shim at all, and a shim's record of access ends with its exec, so a client
  that just finished a long command would count for nothing. Forgetting is
  silent until the sandbox powers off under someone (ADR 0108 §2).
- **Report idle, sleep and wake only once the pool has handed the power
  over** (`ControlPlanePower`). Before that the sandbox powers itself off, and
  a status reporting `idle` to a server that does not act on it describes a
  stop that is not coming; `controlPlanePower: true` is the one field always
  reported, because it is how the pool knows it may hand the power over
  (ADR 26-10-16-337 §8).
- **A boot hands its due wakes over once, and a replaced container loses
  them.** The pool replacing the container right after the wake's boot (an
  upgrade, a changed project configuration) kills the harness that took them,
  and the next boot finds nothing due (ADR 26-10-16-337, Consequences). A
  change that makes replacement after boot more common makes this worse.
- **A confirmed sleep consumes its requests before anything powers down.** A
  frozen sandbox keeps `/run`, and an unconsumed request would put it straight
  back to sleep when it resumes.
- **A thaw is a start for the idle clock.** `Resumed` records activity as the
  agent's own start does for a boot: the wall clock runs on while the cgroup is
  frozen, so a sandbox frozen past its idle timeout is otherwise reported idle
  and stopped within a poll of thawing, losing the memory the freeze kept
  (ADR 26-10-16-420 §4).
- **Reading is never activity.** A status poll, a listing, a title re-sent
  unchanged: none of them may move the idle clock. The pool agent polls every
  sandbox's status every 15 seconds, so anything a read counts as activity
  keeps every sandbox up forever.
- **Output is not activity; a changed screen is.** `screenBuffer.sumText`
  hashes cell content only. Adding styles, the cursor, the scrollback (filled
  on every `CSI 2J`), or raw byte counts to it makes a self-drawn blinking
  cursor or an idle TUI's redraw hold the sandbox up forever. So does
  comparing on a write rather than when a burst ends: a frame split across PTY
  reads is a different screen part-way through. A burst settled by a
  `/status` read is dated to its last write, never to the read (ADR 0124).
- **A live reported status replaces the screen, and only `working` is
  activity.** For an exec whose program reports `working`, `blocked` or
  `idle` over OSC 7501, `autostop` reads that status instead of its screen
  changes: a blocked agent animating a prompt is still a box waiting on a
  person, and counting its screen holds the sandbox up for nobody (ADR
  26-10-16-424 §4). A `done` or `error` record does not replace the screen:
  it is a result the shim keeps after the program that reported it is gone,
  so whatever runs in that terminal next — a build started from the shell —
  reports nothing, and gating on any record would power the box off under
  it. Dropping a record is not activity either.
- **The OSC 7501 probe is answered whatever is attached, in query order.**
  The reply goes into the emulator's response pipe (`SendText` from the
  handler) so it reaches the program before the DA1 answer that follows it —
  Claude Code reads DA1 arriving first as "unsupported" and never reports —
  and `pumpScreenResponses` lets it through while a client is attached, when
  it drops the emulator's other answers, by matching the chunk to
  `programstatus.ReplySequence`. That match holds only while each read of the
  pipe returns exactly one write: a pump that buffers, coalesces, or reads
  less than a whole reply drops it while attached. A second path for the
  reply (a channel beside the pipe) reorders it against DA1.

## The discobox-review skill

- **Every path through the skill that changes the tree ends by writing a new
  review card or clearing the one signed `card`.** discobox-review does not
  carry a card's places when the tree moves (discobox-ai/review ADR
  26-10-21-407, Deferred): the window marks whatever hunks now sit at the old
  line numbers as the core, without saying so. A run that stops at the round
  cap or on a question has edited the tree since an earlier run's card, and is
  exactly when the user is handed the change to judge.

## Private harness capture routes

- Match capture scopes using ogen's `uri.NormalizeEscapedPath` convention,
  preserving encoded slashes. Comparing raw escaped literals misses routes such
  as `%68arness-config-capture` that ogen dispatches as `harness-config-capture`.
  This holds for every route a scope is told by its whole path (`routedPath`):
  the autostop and cache-clear routes were matched on the raw escaped path, so
  `cache/%63lear` reached the handler with no scope required at all.
  Exercise the authenticator and generated router together when changing these
  scopes; helper-only tests cannot detect disagreement between the two.

## Harness capture baselines

- A versioned exporter response is not necessarily a complete baseline. Partial
  diagnostics can omit valid delivered settings during bundle startup or parser
  failure. Normalize into a temporary result, validate completeness before
  caching it, and retry incomplete cached projections from the original
  delivered files. Otherwise every omitted baseline file becomes a false local
  change, even after the exporter recovers. Cover restart with a persisted
  version-1/partial baseline and an already-created false candidate.

## Clearing caches

- `boot.ClearCaches` empties declared cache paths, and a data volume may be
  declared inside one: `/nix` is a cache whose profiles, gcroots and daemon
  socket are data. Keep every non-cache volume path, and what leads to it,
  when emptying a cache. Why: the Docker clear empties `/.discobox/cache`,
  where no data lives, so a machine's clear is the first to walk a cache that
  holds data.
- A raw secret's value (ADR 26-10-27-840) is read from its file under
  `/run/discobox/raw-secrets` by whatever starts the process, into that
  process's environment and nothing else. A new start path passes
  `RawEnv` through rather than putting the value in `Env`: `Env` is recorded
  in the exec's runtime file, answered by the exec API, posted in the
  inventory and written into the shim's arguments.
- A new way to start a process goes through `execs.Manager` (or calls its
  `awaitRawSecrets` first). The hold for raw secrets lives there, so a start
  path of its own would launch before the pool has answered for the raw
  secrets the applied document names, and run without them for good.

## Bridge configs

- A bridge config (`bridge.json` and its siblings in the proxy material
  directory) is rendered and decoded as `proxy/bridge.File`, and a reader
  dials the pool through `File.Dial`, whose `bridge.DialConfig` it holds
  whole — the forwarder's `bridge.Config.Pool`, the DNS stub's
  `dnsstub.Config.Pool` — never a struct of its own, nor the dial's fields
  copied out one by one. Why: four
  hand-written copies once each had to learn `address`, and a reader that
  missed it would decode the file without error and dial
  `discobox-pool-proxy`, which nothing on a machine resolves — a bridge that
  fails on machine pools alone.
