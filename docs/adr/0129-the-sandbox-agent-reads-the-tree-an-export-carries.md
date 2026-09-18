# 0129 — The sandbox agent reads the tree an export carries, and the image says what of it stays behind

- **Status**: Accepted
- **Date**: 2026-09-18
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
  [0033](0033-user-resolution-is-one-layered-resolver-with-declared-gaps.md),
  [0038](0038-terminal-identity-is-the-exec-id-terminals-revive-in-place.md),
  [0058](0058-a-push-delivered-source-has-a-pool-side-origin.md),
  [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md),
  [0085](0085-the-nix-seed-stamp-names-the-store-that-seeded-it.md),
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md),
  [0107](0107-homebrew-is-image-content-on-an-overlay-handed-to-a-group.md),
  [0123](0123-a-discobox-is-exported-as-its-spec-and-its-durable-tree.md), and
  [0126](0126-remote-sandboxes-connect-out-to-their-pool.md).
  On acceptance, this narrows 0123 §1's `tree/data/` to what the image lets
  travel, and answers 0126 §6's open question of how a stopped sandbox's tree
  is read. The `.dbox` format and its version do not change.
  It also narrows what ADR 0075 §2 and ADR 0107 keep per-sandbox: they persist
  across restarts but no longer across an export.

## Context

An export's durable tree is produced today by the pool agent
(`pool-agent/sandboxruntime/tree.go`): it walks `data/`, `sources/` and
`origins/` under the sandbox root on the pool host, as root, and streams the
tar. That has three problems, and they share a cause — the reader is not the
party that knows what the tree is.

- **Nested container state travels, and travels wrong.** The base image
  declares `/var/lib/docker` and `/var/lib/containerd` on `data`, so every
  export carries the nested daemon's whole image and snapshot store. That is
  gigabytes of pulls and builds the destination can redo, and it cannot be
  carried faithfully by this exporter anyway: the containerd overlayfs
  snapshotter records deletions as 0/0 character devices and opaque
  directories as `trusted.overlay.*` xattrs, and the walk skips device nodes
  and writes no xattrs. A restored store silently resurrects every file a
  later layer deleted.
- **Only the sandbox can say where an image-declared path lives.** A declared
  volume resolves against the sandbox user's home and uid, and those are
  resolved inside the sandbox, against the image's own account database
  (ADR 0033). The pool agent knows the home only when the create request
  stated it, so a pool-side reader cannot reliably locate a declared
  `%HOME%/...` path in the tree at all.
- **ADR 0126's pool cannot see the tree.** A remote compute sandbox keeps
  `data` and `sources` on its own private disk. 0126 §6 keeps the export
  contract and the stopped-sandbox guarantee but leaves how the stopped tree
  is read to each backend.

It is also the read-side twin of the problem `readTree` solves with `os.Root`
on import: root on the pool host opening, one by one, paths the sandbox wrote.
The walk does not follow symlinks, but it opens each file by name after the
walk sized it, and nothing between those two steps is the sandbox's own
namespace.

## Decision

### 1. `data` and `sources` are read by the sandbox agent, in an export mode

`discobox-sandbox-agent export` is a mode of the agent the image already
carries. It runs as PID 1 of a one-shot environment started from the
sandbox's pinned image, and it never starts systemd, the harness, or a
service — so it is quiesced by construction, and nothing the user runs is
running while the tree is read. An export still refuses a running sandbox
(ADR 0123 §2); the export mode is how a *stopped* one is read.

It reads `sandbox.json` from the config root and resolves the sandbox user
and the image's declared volumes exactly as boot does, then writes a tar of
`data/` and `sources/`, names relative to the sandbox tree as today, ending in
its own SHA256SUMS (tarsums). It walks the primary volumes
(`/.discobox/data`, `/.discobox/sources`), not the wired targets, so what it
reads is what the pool stores. A sandbox whose create never wrote
`sandbox.json` has no declared volumes to honour, and its tree travels whole.

The pool agent treats that stream as untrusted input: it verifies its
SHA256SUMS, refuses any entry outside `data/` or `sources/`, re-emits it, and
appends `origins/` — which is pool-owned in both runtimes (ADR 0058, 0126
§4) — before writing the SHA256SUMS of the tree it serves. Everything above
the pool agent is unchanged.

- **Docker runtime.** The pool agent runs a one-shot container from the image
  ID the sandbox runs, with its data and sources roots bound **read-only**,
  its config root read-only, no cache, secrets, origins or source-data mounts,
  no network, and not privileged. The tar is the container's stdout.
- **Remote runtime (0126).** The same subcommand, started by the backend
  against the stopped disk, is the "quiesced transfer" 0126 §6 asks a backend
  to provide. How its stdout reaches the pool agent is the backend's
  transport; the producer and its format are fixed here.

### 2. A `data` volume can declare that it does not travel

A declared volume gains `excludeFromExport`:

```json
{ "path": "/var/lib/docker", "volume": "data", "excludeFromExport": true, "uid": 0, "gid": 0, "mode": "0711" }
```

- It excludes the volume's whole backing directory on the data root —
  `volumeDir`, including an overlay's `upper` and `work` — and every declared
  path nested beneath it.
- Absent means the path travels. Only a `data` path may set it: a cache path
  never travels, so the claim would be meaningless, and it is refused where
  the control plane reads the label, as `scope` is (`ValidateVolumeScope`).
- It crosses the same hops `scope` does — image label, harness config row,
  both `HarnessVolume` schemas, `sandbox.json` — absent staying absent at
  each.
- A harness image can set it on its own paths; layers merge volumes by path,
  so a later layer re-declaring a path decides for it.

On import an excluded directory is simply absent, and boot creates it empty
with its declared ownership and mode, as on any first boot.

**The rule for choosing: an export carries the discobox's work, not what was
installed into it.** The container's writable layer already stays behind
(ADR 0123 §1), so a restored discobox has never had `apt-get install`'s
results; a `data` path that holds the same kind of thing — a package manager's
installs, a daemon's rebuildable state — is excluded to match. A restore with
those gaps is an accepted wart: the harness, a declared service, or the user
installs again what is needed, as they would after an upgrade. A path that
holds the user's work, or state without which the discobox is not the same
discobox, travels.

The base layer (`sandbox-agent/image.json`), entry by entry:

| Path | Export | Why |
| --- | --- | --- |
| `%HOME%` | travels | The user's work. |
| `/var/lib/discobox` | travels | The sandbox agent's own state: terminal identity, whether the primary terminal has launched (so a restore resumes the harness rather than starting it fresh, ADR 0038), exec history. |
| `/var/lib/docker`, `/var/lib/containerd` | excluded | The nested daemon's images, containers and volumes: rebuildable, and not carried faithfully (Context). |
| `/nix/var/nix/profiles`, `/nix/var/nix/gcroots` | excluded | Symlinks into `/nix`, a cache path that never travels, so on the destination they name store paths that may not exist. `discobox-nix-seed` seeds the default profile again (ADR 0075, 0085). |
| `/home/linuxbrew/.linuxbrew` | excluded | `brew install`'s results, the same kind of thing as `apt-get install`'s. The image's own Homebrew is the overlay's lower layer and is there after a restore (ADR 0107). |

**Nothing that travels may vouch for an excluded path on its own.**
`discobox-nix-seed` today stamps the per-sandbox seed in
`/var/lib/discobox/nix-seeded`; if that stamp travelled while the profiles
stayed behind, the restored sandbox would skip seeding and have no default
profile. The per-sandbox stamp therefore moves into the tree it describes,
`/nix/var/nix/profiles/.discobox-seeded`, so it and the profiles are absent
together and a restore re-seeds exactly as a first boot does. Its content is
unchanged (ADR 0085).

- **Not `/nix/.<anything>`.** `/nix` is the pool-shared cache, so a stamp there
  is seen by every sandbox in the pool — that is the store stamp's job. A
  per-sandbox one there would tell the next sandbox its profiles are seeded,
  which is the dangling-default-profile failure of ADR 0085.
- **Nix ignores it.** The script's objection to a stamp inside `/nix` was that
  something scanning a nix directory might take it for a profile or a gcroot.
  Nix treats a regular file under `gcroots` as a root only when its name parses
  as a store path, and lists generations only by `<profile>-<N>-link`; checked
  against nix 2.35, the dotfile is neither a root nor a generation, and GC and
  `--delete-generations` leave it alone.
- **An existing sandbox re-seeds once** on the image that moves the stamp: it
  finds no stamp at the new path and copies the seed's `profiles` and
  `gcroots` again. That is the copy every image rebuild already triggers, since
  a new seed id is not in the stamp either.

### 3. An image whose agent predates the export mode is refused by name

The export mode lives in the image, so a sandbox pinned to an older one has
an agent that does not have it — and that agent reads an unknown argument as
an ordinary start, so it cannot be probed. The base image therefore
advertises the mode in a label set beside the `10-sandbox-base` layer, and the
pool agent checks the pinned image for it before starting anything. Without
it the export is refused with `discobox admin box upgrade` named as the
remedy. An import re-pins the image to the destination's harness config
(ADR 0123 §1), so upgrading first gives up nothing the transfer was keeping.

## Alternatives rejected

**The pool agent resolves the exclusions from `sandbox.json` itself.** The
smallest change, and it covers `/var/lib/docker`. But it cannot locate a
`%HOME%` path whenever the create request did not state the home, and it
keeps the reader on a host that, under ADR 0126, cannot see the tree.

**Boot records the resolved exclusions in `data/` for the pool agent to
honour.** It fixes resolution, but keeps the pool agent reading as root on
the pool host, and is the same dead end under 0126.

**Export a running sandbox through its live agent API.** No new mode to
start, but it gives up the quiesce ADR 0123 §2 exists for.

**Hard-code the nested daemon's directories in the exporter.** They are the
image's knowledge — the image declares them — and a harness image with
podman or its own state directory would need an exporter change.

**Carry the nested store faithfully**, with xattrs and whiteout devices. The
store is tied to the source's snapshotter and kernel, and is a cache of what
the destination can pull and build again. Making the exporter a container
storage migrator buys back a cache.

**Fall back to the pool-side walk for images that predate the mode.** Two
walkers and two exclusion implementations, one of which cannot exist under
0126, to spare an upgrade the import performs anyway.

## Consequences

- Nothing in the nested Docker daemon travels: images, containers, build
  cache, and **named volumes**. Data a user keeps in a Docker volume inside
  the discobox is lost on export and transfer, and the export documentation
  and `admin box export` help must say so.
- An export starts a container, and needs the pinned image present on the
  pool (pulled again if reaped), so it takes longer to begin.
- On export, the pool agent no longer opens a path the sandbox wrote. It
  copies and checks a byte stream.
- Existing `.dbox` files import unchanged, docker state included; the format
  version stays 1.
- A restored discobox is missing what was installed into it with `brew`,
  `nix profile`, or `apt-get`, and `~/.nix-profile` dangles until nix writes
  the user's profile again. Accepted, per §2's rule.
- The exporter still drops whiteouts and xattrs for paths that travel, so a
  travelling `data` path wired as an overlay would lose its deletions. No
  base-image path that travels is an overlay once Homebrew stays behind; an
  image that makes one travel inherits the gap.

## Deferred

- **Restore through the sandbox agent.** Import keeps the pool-side restore
  confined by `os.Root`, which is sound for the Docker runtime. Revisit when
  the first ADR 0126 backend is built, since that pool cannot write the tree
  either and 0126 §6 requires both halves restored before start.
- **Excluding paths that are not declared volumes.** Revisit if an image
  needs to exclude a path it cannot reasonably declare as a `data` volume.
