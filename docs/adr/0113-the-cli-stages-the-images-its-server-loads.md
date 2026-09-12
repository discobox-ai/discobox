# 0113 — A release CLI stages its server's images, and a pool loads them before it pulls

- **Status**: Accepted
- **Date**: 2026-09-11
- **Amends**: [0099](0099-the-cli-downloads-the-server-it-starts.md) §3 — a manifest also lists the images its server runs, by reference and without a digest. §3's "a URL and a digest or neither" still governs every *asset*; §1 below says why an image is a different kind of entry.
- **Relates to**: [0069](0069-staging-pool-images-is-a-condition.md), whose staging condition is unchanged and now usually loads rather than pulls; [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) §6, whose digest pin every loaded image has to satisfy.

## Context

A first run downloads twice, and the second download is the long one.

The CLI stages the server — about 94 MB, verified, narrated on the launch line
(ADR 0099). Then the server starts, a pool comes up, and the pool's own Docker
daemon pulls gigabytes from the registry: the pool-agent image while the pool
is being brought up, then the sandbox-agent image and the three built-in
harness images as ADR 0069's staging condition. On every VM backend that pull
happens inside the guest, over the guest's network, after the user has already
been told the server started.

It also happens once per pool rather than once per machine. Each project gets
its own pool with its own daemon, so a second project pulls the same gigabytes
again, and so does a pool whose data disk was deleted.

The two downloads are the same kind of thing — release content the CLI can
name in advance — but only the first is fetched where the user is watching, by
the process that knows it is a first run.

Four facts shape the design, and each was checked against a daemon rather than
assumed:

- **A pool daemon enforces the pin through `RepoDigests`.** A harness config
  records the registry digest of its image (an index digest for our multi-arch
  images) and the pool agent refuses to launch a sandbox whose image does not
  carry it (`imageMatchesPinDigests`). A plain `docker load` of a
  `docker save` archive does not produce that digest, so a naive cache would
  turn "slow" into "the pinned image is not available on this pool".
- **The containerd image store keeps the digest an archive gives it.** On
  Docker 29.8 (containerd store), an archive whose `index.json` names the
  registry's *original* index — with only one platform's manifest, config and
  layers present — loads as that index: image ID and `RepoDigests` are the index
  digest, and the image runs. `docker save --platform` does not do this; it
  writes the platform manifest as the top-level entry and loses the index.
- **The classic store does not, but one pull by digest restores it for free.**
  The guest image installs Debian 13's `docker.io`, 26.1.5, whose default is the
  classic `overlay2` store. On it (and on Docker 29.8 forced classic) the same
  archive loads with the config digest as ID and no `RepoDigests`. A following
  `docker pull repo@<index digest>` transfers no layers — moby's puller returns
  early when the image config already exists — and records the digest.
- **Switching the guest to the containerd store is not an option.** The store
  a daemon starts with decides which images and containers it can see; changing
  it on an existing pool's data disk hides every sandbox container on it.

## Decision

### 1. The manifest lists the images its server runs, by reference

`serverstage.Manifest` gains `images`: the pool-agent image, the sandbox-agent
image, and the built-in harness images, as the references the server is linked
with (`SERVER_LDFLAGS` in `release:binary`) — the same registry and release tag,
generated from the same Taskfile variables, so the list and the server cannot
name different releases.

A reference, not a digest, for the reason ADR 0099 §3 pins
`DefaultPoolImage` to a tag: `release:images` runs in a job parallel to the one
that links the binaries, so no image digest exists when the CLI is linked. That
is no weaker than what it replaces. The pool would otherwise resolve the same
tag against the same registry; here it is resolved once, by the CLI, and every
byte below it — index, manifest, config, layers — is content-addressed and
checked against its digest as it is written.

The list is a head start and not a contract. An image the server needs that the
list does not name is pulled by the pool exactly as it is today.

### 2. The CLI stages them into a content-addressed OCI layout

Beside the staged servers, in `<state>/discobox/images`: an OCI image layout —
`oci-layout`, `index.json`, `blobs/sha256/` — so the cache is inspectable with
standard tools and nothing about it is private to this code.

- **One platform:** `linux/<host arch>`, the platform every local pool runs on
  (the same rule `harnessconfigs.poolPlatform` states). Pulling every platform
  doubles the bytes for pools that cannot exist on this machine.
- **Only what is missing:** a blob already in the layout is not fetched again, so
  a new release costs its changed layers, and the base layers the five images
  share are fetched once.
- **Verified while written, installed by rename:** every blob is hashed as it
  arrives and renamed into place only when its digest matches; an index entry is
  written only once every blob it reaches is present. An interrupted stage
  leaves blobs worth keeping and nothing that claims to be complete.
- **The registry's own index is kept**, byte for byte, because its digest is
  the pin (§4).
- **A stdlib client.** The fetch is the OCI distribution API with an anonymous
  bearer token — what a public release registry needs — written in the root
  module. ADR 0099 took go-containerregistry out of `cli/go.mod`, and a few
  hundred lines of verified HTTP are not a reason to bring it back.

Two things stage images: the autolaunch, immediately after it has resolved
the server and before it starts one, and `discobox admin server stage`, which
provisions everything a later autolaunch would fetch. `discobox admin server`
in the foreground stages only the binary: whoever runs it is starting a server
on purpose, and its pools load whatever is already staged and pull the rest.
Once the layout is complete a stage is a few `stat` calls, and a server that is
already running costs nothing, as before.

**Staging comes before a running server is stopped.** An autolaunch replaces a
local server that is older than the CLI, and the download is now minutes long,
so `endpoint.EnsureRunning` resolves the command to start — server and images —
while the old server is still serving, and only then asks it to shut down. The
gap a user sees is the switch-over, not the download.

It is narrated on the launch line in stageLine's grammar, so the first run
reads as one sequence: *Downloading server*, *Downloading images (2 of 5):
discobox-harness-codex — 120 MiB of 1.1 GiB*, *Starting server*.

A failure is a lost head start, never a failed launch (ADR 0069): the autolaunch
says the images will be downloaded when first needed and starts the server.
`discobox admin server stage` fails, because staging is what it was asked to do.

After a successful stage, index entries that are not in the current set and have
not been staged for 24 hours are dropped, and blobs no remaining entry reaches
are deleted — only once they are old enough to be nobody's download in flight,
the rule `serverstage.sweepAbandoned` already applies. 24 hours matches ADR
0040's image retention: long enough that alternating between two installed
versions does not re-download either.

### 3. The CLI tells the server where the layout is

The server's configuration file (ADR 0096) gains `imageCacheDir`, with
`DISCOBOX_IMAGE_CACHE_DIR` as its environment override like every other key.
The CLI sets the variable on every server it runs — the autolaunch and
`discobox admin server` — unless its own environment already names one, which
is then also where it stages. Unset means no cache, and the server behaves exactly as it does today;
that is every server nothing launched through a CLI.

### 4. A pool loads from the layout before it pulls

`dockerworker.Engine.ensureImageRef` — the one path behind both the pool-agent
image and `StageImages` — gains a step between "absent from the daemon" and
"pull": when the layout holds the reference for the daemon's platform, an
archive is streamed from it into `ImageLoad`.

The archive carries the registry's index as the single entry of `index.json`,
annotated with the reference, plus a docker `manifest.json` naming the
platform's config and layers. The containerd store imports it as the index, so
the pin holds as loaded. The classic store reads `manifest.json`; the engine
tells the two apart by the daemon's reported `driver-type`, and on a classic
daemon follows the load with a pull of `repository@<index digest>` — no layers,
and the digest the pin needs. If that pull fails, the loaded tag is removed
again: a present image that can never satisfy a pin is worse than an absent one,
because nothing would ever pull over it.

Any failure along the way falls through to the pull that happens today. A load
never makes staging fail that the pull would not also have failed.

**Only for a daemon on this machine.** The engine's configuration carries the
layout only from the providers whose daemon is local — `docker`, `libkrun`,
`vz`, `wslc`. A DigitalOcean or `exec` daemon pulls from the registry, which is
closer to it than this machine's disk is. This is a nil field, not an optional
interface: every engine has the step, and some are given nothing to load.

### 5. Loading is narrated as loading

`PoolImageStage` gains `loading`, and `PoolProvisionPhase` gains
`loading_pool_image`. The CLI and the launcher window say *Loading images* and
*Loading runtime image*. A line that said *Downloading* would tell a user who
has just watched the download finish that it is happening again.

## Consequences

- A first run downloads everything on the launch line, before the server
  starts, and the pool's own staging becomes local disk and CPU: loading and
  extracting rather than fetching. A second project's pool, and a pool rebuilt
  on a fresh disk, load too.
- **Every command that autolaunches pays it, `discobox ps` included.** That is
  the choice being made: a first `ps` that shows a byte counter for a few minutes
  once per release, rather than a pool that pulls later out of sight. It is
  deliberately unlike ADR 0069's wait, which `ps` skips because it has no
  denominator and read as a hang.
- The images are on disk twice: compressed in the layout and extracted in each
  pool's daemon. The layout is bounded by §2's pruning to about one release's
  set, a few gigabytes.
- `discobox admin server stage` now provisions a machine for offline use in full,
  images included.
- The layout holds release content for one reference set at a time. A
  development build carries no manifest and its images are `:local`, so nothing
  changes for `task dev`.
- The manifest schema changes, so the CLI decoding it and the tool encoding it
  move together, as with ADR 0106.

## Rejected alternatives

**`docker save` archives published as release assets.** It fits ADR 0099's
digest-per-asset model exactly, and it throws away what the registry already
does well: five images that share a base would carry it five times, a release
would re-download every unchanged layer, and the harness images approach
GitHub's per-asset size limit. The registry is already a content-addressed
store; the layout is a copy of the part of it this machine needs.

**The server fetches the images at startup.** It has go-containerregistry and a
registry keychain already. But ADR 0069 took staging out of server startup
because the API was unreachable for as long as it took, and the CLI is the
process watching a terminal: it would be reduced to polling a server that is
busy downloading rather than narrating a download it is doing.

**A pull-through registry on the host.** Pools would pull as they do now, from
a cache. Docker's `registry-mirrors` apply only to Docker Hub, so every guest
would need its image references rewritten or an insecure registry configured,
and a host port opened — for a result the layout gets with an `ImageLoad`.

**Enable the containerd store in the guest image.** It would make the classic
path unnecessary. It also hides every image and container on an existing pool's
data disk the first time that pool boots the new guest.

**Fetch all platforms.** Harmless in correctness, and double the bytes for
nothing: a local pool is the host's architecture.

## Deferred

**Digests in the manifest.** Revisit if `release:images` ever finishes before the
binaries are linked; then the manifest can state a digest per image and the
server can be pinned to digests rather than tags.

**The VM guest image.** `discobox-vm` (and libkrun's kernel image) is fetched by
the server's `guestimage` resolver into its own cache during the first pool
start — hundreds of megabytes the layout could hold too. It is left out because
it has a different consumer and its reference is a digest in server code rather
than a Taskfile variable. Revisit when it is the largest wait left in a first
run.
