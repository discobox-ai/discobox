# 0113 — A release CLI stages its server's images, and a pool loads them before it pulls

- **Status**: Accepted
- **Date**: 2026-09-11
- **Relates to**: [0099](0099-the-cli-downloads-the-server-it-starts.md), whose staged server is what names the images; [0069](0069-staging-pool-images-is-a-condition.md), whose staging condition is unchanged and now usually loads rather than pulls; [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) §6, whose digest pin every loaded image has to satisfy; [0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md) §3, whose digest-pinned guest image is staged too.

## Context

A first run downloads twice, and the second download is the long one.

The CLI stages the server — about 94 MB, verified, narrated on the launch line
(ADR 0099). Then the server starts, a pool comes up, and the rest arrives
behind it: on macOS the VM guest image, hundreds of megabytes fetched by the vz
driver before a machine can boot; then the pool-agent image, pulled by the
pool's own Docker daemon while the pool is brought up; then the sandbox-agent
image and the three built-in harness images, as ADR 0069's staging condition.
On every VM backend the image pulls happen inside the guest, over the guest's
network, after the user has already been told the server started.

The image pulls also happen once per pool rather than once per machine. Each
project gets its own pool with its own daemon, so a second project pulls the
same gigabytes again, and so does a pool whose data disk was deleted.

All of it is release content that can be named in advance, but only the server
binary is fetched where the user is watching, by the process that knows it is a
first run.

Five facts shape the design, and each was checked against a daemon or the code
rather than assumed:

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
- **Only the server knows the VM images a machine needs.** The guest and kernel
  references are digests pinned in server source on their own release line
  (ADR 0062 §3), and which of them a machine boots depends on the provider the
  server installs by default on that OS: vz on macOS boots the guest, while
  Linux defaults to the host's Docker and Windows to wslc, and neither boots
  one.

## Decision

### 1. The server names the images it runs

`discobox-server images` prints, one per line, the images a first run on this
machine will want: the pool-agent image, the default sandbox image, the
built-in harness images, and the boot images of this OS's default provider
(`providers.DefaultBootImages`: the vz guest on macOS, nothing elsewhere). It
reads the server's configuration as `Run` does, so an overridden pool or
sandbox image is the one named, and it leaves out local tags that exist on no
registry. It starts nothing.

The CLI asks the server it has just staged, before starting it. That is the
only binary it asks: one staged from the manifest this CLI was linked with is
the same release as the CLI and so has the command. A binary named with
`--binary`, a sibling, or a server staged from a `--manifest` of another version
is not asked, because a server from before this command takes the argument as
nothing and starts serving.

The list is a head start and not a contract. An image the server needs that it
did not name is fetched when it is needed, exactly as it is today.

### 2. The CLI stages them into a content-addressed OCI layout

Beside the staged servers, in `<state>/discobox/images`: an OCI image layout —
`oci-layout`, `index.json`, `blobs/sha256/` — so the cache is inspectable with
standard tools and nothing about it is private to this code.

- **One platform:** `linux/<host arch>`, the platform every local pool — and
  every VM guest — runs (the rule `harnessconfigs.poolPlatform` states).
  Pulling every platform doubles the bytes for pools that cannot exist here.
- **Only what is missing:** a blob already in the layout is not fetched again, so
  a new release costs its changed layers, and the base layers the images share
  are fetched once.
- **Verified while written, installed by rename:** every blob is hashed as it
  arrives and renamed into place only when its digest matches; an index entry is
  written only once every blob it reaches is present. An interrupted stage
  leaves blobs worth keeping and nothing that claims to be complete.
- **The registry's own index is kept**, byte for byte, because its digest is
  the pin (§4).
- **A stdlib client.** The fetch is the OCI distribution API with bearer tokens,
  written in the root module. ADR 0099 took go-containerregistry out of
  `cli/go.mod`, and a few hundred lines of verified HTTP are not a reason to
  bring it back. The CLI's fetches are anonymous — a release's images are
  public; the server passes its registry keychain (§5).

Two things stage images: the autolaunch, immediately after it has resolved
the server and before it starts one, and `discobox admin server stage`, which
provisions everything a later autolaunch would fetch. `discobox admin server`
in the foreground stages only the binary: whoever runs it is starting a server
on purpose, and its pools load whatever is already staged and fetch the rest.
Once the layout is complete a stage is a few `stat` calls, and a server that is
already running costs nothing, as before.

**Staging comes before a running server is stopped.** An autolaunch replaces a
local server that is older than the CLI, and the download is now minutes long,
so `endpoint.EnsureRunning` resolves the command to start — server and images —
while the old server is still serving, and only then asks it to shut down. The
gap a user sees is the switch-over, not the download.

It is narrated on the launch line in stageLine's grammar, so the first run
reads as one sequence: *Downloading server*, *Downloading images (2 of 6):
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

### 3. The server is told where the layout is, and always has one

The server's configuration file (ADR 0096) gains `imageCacheDir`, with
`DISCOBOX_IMAGE_CACHE_DIR` as its environment override like every other key.
It defaults to `<cacheDir>/images`, because the server fetches through it too
(§5) whether or not a CLI staged anything. The CLI sets the variable on every
server it runs — the autolaunch and `discobox admin server` — unless its own
environment already names one, which is then also where it stages.

### 4. A pool loads a container image from the layout before it pulls

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

Any failure along the way falls through to the pull that happens today.

**Only for a daemon on this machine, and the driver says which.** Every engine
is given the store; what decides a load is the Docker client lease, which
carries a `DaemonLocality` its driver must pass to construct one. A daemon
elsewhere is closer to its registry than to this machine's disk, so nothing is
loaded into it.

It is the driver's answer because for two backends it is not a property of the
provider at all: the docker provider's daemon is wherever its `host` points,
which may be another machine, and the exec provider resolves an endpoint per
pool that may be a socket here or an `ssh://` target anywhere. `vz`, `libkrun`
and `wslc` boot a VM here and always answer this machine; DigitalOcean dials a
droplet over SSH and always answers elsewhere; the two configurable ones answer
from the host URL, by the same socket-transport rule the docker provider
already applies to bind mounts (`DaemonLocalityForHost`).

A required constructor argument rather than a field with a default, so a new
backend is asked the question by the compiler rather than silently getting the
wrong answer — and `DaemonElsewhere` is the zero value, because the wrong
answer that way costs only speed.

### 5. Providers are handed the store, and a VM guest is fetched through it

A provider does not learn the layout. It is handed `*imagecache.Layout`
(`ServerDefaults.ImageCache`), whose `Fetch` returns an image for a platform —
from disk when every blob is there, downloading and verifying what is missing
into the same layout when it is not — and whose `Image` reads its blobs back.
The layout's format is the `imagecache` package's business alone.

`guestimage.Resolver`, which the vz and libkrun drivers resolve their boot
artifacts through, fetches through that store instead of reading the registry
itself. A guest image a CLI staged is therefore extracted from local blobs; one
it did not is downloaded into the store, where the next pool and the next stage
find it. The extracted artifacts stay in the provider's own cache keyed by
digest, and for a digest-pinned reference that cache is consulted first, so a
machine that already extracted its guest downloads nothing. A tag reference is
revalidated against its registry on every resolution, as it always was, and
the server's registry keychain authorizes a private override exactly as it did.

### 6. Loading is narrated as loading

`PoolImageStage` gains `loading`, and `PoolProvisionPhase` gains
`loading_pool_image`. The CLI and the launcher window say *Loading images* and
*Loading runtime image*. A line that said *Downloading* would tell a user who
has just watched the download finish that it is happening again. A guest image
fetched from local blobs still reports `fetching_vm_image`, with no bytes to
count.

## Consequences

- A first run downloads everything on the launch line, before the server
  starts — on macOS the guest image included — and a pool's own start becomes
  local disk and CPU: extracting and loading rather than fetching. A second
  project's pool, and a pool rebuilt on a fresh disk, load too.
- **Every command that autolaunches pays it, `discobox ps` included.** That is
  the choice being made: a first `ps` that shows a byte counter for a few minutes
  once per release, rather than a pool that pulls later out of sight. It is
  deliberately unlike ADR 0069's wait, which `ps` skips because it has no
  denominator and read as a hang.
- The images are on disk twice: compressed in the layout and extracted in each
  pool's daemon or guest cache. The layout is bounded by §2's pruning to about
  one release's set, a few gigabytes.
- `discobox admin server stage` now provisions a machine for offline use in full,
  images included — for the platform it runs on. Staging a `--manifest` for
  another platform stages no images, because that server cannot be run here to
  name them.
- On a classic-store pool the load needs the registry for two manifests, so a
  VM pool with no network at all still cannot finish bringing its images up.
- A development build carries no manifest and autolaunches nothing, so nothing
  changes for `task dev`; its server fetches guest images through
  `<cacheDir>/images` like any other.

## Rejected alternatives

**The manifest lists the images.** It was this ADR's first form, and it works
for the tag-pinned agent and harness images, which the Taskfile already names.
The VM images are digests pinned in server source, and whether a machine needs
one at all is a fact about the server's default provider on each OS. Listing
them in the manifest meant a second copy of both facts in the release build,
kept in step by nothing; asking the server removes the copy.

**`docker save` archives published as release assets.** It fits ADR 0099's
digest-per-asset model exactly, and it throws away what the registry already
does well: five images that share a base would carry it five times, a release
would re-download every unchanged layer, and the harness images approach
GitHub's per-asset size limit. The registry is already a content-addressed
store; the layout is a copy of the part of it this machine needs.

**The server fetches the images at startup.** It has a registry client and a
keychain already. But ADR 0069 took staging out of server startup because the
API was unreachable for as long as it took, and the CLI is the process watching
a terminal: it would be reduced to polling a server that is busy downloading
rather than narrating a download it is doing.

**A pull-through registry on the host.** Pools would pull as they do now, from
a cache. Docker's `registry-mirrors` apply only to Docker Hub, so every guest
would need its image references rewritten or an insecure registry configured,
and a host port opened — for a result the layout gets with an `ImageLoad`.

**Each provider keeps its own image cache, and learns where the CLI's is.**
That is an agreement about a directory format between the CLI and every
provider that boots something, which is the coupling §5 exists to avoid.

**Enable the containerd store in the guest image.** It would make the classic
path unnecessary. It also hides every image and container on an existing pool's
data disk the first time that pool boots the new guest.

## Deferred

**libkrun's images are fetched through the store but not staged.** libkrun is
not Linux's default provider, so the CLI does not name its guest and kernel;
a libkrun pool's first start downloads them into the store, narrated as today.
Revisit if libkrun becomes Linux's default.
