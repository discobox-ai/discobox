# Release manifests

A manifest names runtime images by role: sandbox base, VM guest and libkrun
kernel. The sandbox base is what every sandbox container and every Docker
pool's runs (ADR 26-10-24-953 §2); a release cut before the Docker driver
owned one names none, and runs a Docker pool's container from its
`Images.PoolAgent` image instead, which a release from then on names none
of: `Validate` takes either. Beside them, `Bundles` names the platform
bundle and each harness's bundle by slug (ADR 26-10-16-423 §2): the server
composes every harness's manifest from them and every box applies them.
`Bundles.Agent` is the agent bundle, the sandbox agent alone, which whoever
makes a box places in it, and `Bundles.PoolAgent` the pool bundle, a pool's
box's one bundle (ADR 26-10-24-953 §2); a release cut before either names
none, and still parses. These name bundles by role for the server to compose
a box's ordered list from; nothing past the server sees the roles. A server
runs only on a release that names what a pool's box is made of — the sandbox
base image, the agent bundle and the pool bundle (`HasPoolBox`) — and refuses
any other at startup. No
harness is an image (§10), so a release from bundles on names no sandbox-agent
or harness image; `Images.SandboxAgent` and `Images.Harnesses` are what a
release from before bundles ran its sandboxes on, kept so the CLI still reads
and stages an older server's manifest. `References` derives the host cache's
staging list from the image roles. Bundles are not staged: the cache feeds
pool daemons, and a pool's daemon never holds a bundle — the server serves
them to pools by blob (§13). Never add a separately maintained preload list.
The VM/kernel retain their independent release pins; container image tags name
the Discobox release. Windows supplies its own guest through WSL Containers.

`clients` and `servers` carry verified binary descriptors per platform: version,
OS, architecture, command, download URLs, byte size, SHA-256, and execute bit.
`revision` identifies the source commit. Image-only manifests remain usable for
local development and as intermediate build metadata. The complete published
manifest contains both binaries for each platform.

The manifest version describes its artifacts, not the version of a development
binary using it. An explicit `--binary` / `DISCOBOX_SERVER_BINARY` wins; otherwise
selecting a release manifest instructs the CLI to stage that release's server,
even when a development server exists beside the CLI. No binary is relabeled
with the release's version, and a dev CLI is never replaced by the listed CLI.

`DISCOBOX_RELEASE_MANIFEST` names a local JSON file in both processes. Invalid
or incomplete files fail visibly. Server configuration loads it once at startup,
overrides individual image settings, and disables development image sync. VM
providers also bypass local guest builds when this manifest is selected.
Provider image overrides are superseded; custom user-created harness configs
remain user-owned, and existing sandboxes retain their pinned bundles. A release
from before bundles names none and still parses — the CLI reads older
servers' manifests — but a server refuses to load one: it has no bundle to
read any harness's manifest from, so `dev:released-images` needs a release
from bundles on.

The CLI can stage directly from the manifest without executing a development
server to discover images. Direct server startup stages the same image set
before initializing services, while its startup endpoint reports the download.
Pool provisioning subsequently imports cached container images before making a
pool available. Harness seeding reads metadata from the selected release's
bundles and fails startup if inspection fails. The manifest supplies the seed set even when the
development binary knows about additional harnesses.

`discobox-server manifest` exports the compiled image inventory. The release
build first signs and hashes the server and links that download descriptor into
the CLI. After the CLI's final link, it writes the full platform metadata to
`discobox-manifest-<os>-<arch>.json`. A CLI cannot embed its own final digest;
the full inventory is an external release artifact.

`release:manifest` merges all platform results into `release.json` before
publication. It rejects mismatched versions, source revisions, image sets,
duplicate platforms, and missing CLI/server pairs; the image and bundle sets
must agree. The existing binary download
manifest remains the compact embedded server bootstrap descriptor.

## Development

Inside the Nix development shell:

```sh
go tool task dev:released-images MANIFEST=releasemanifest/examples/v0.8.0.json
```

In another terminal, use `./build/discobox run`. The development loop builds and
hot-reloads local binaries and starts no Docker image watcher. Both binaries
keep their development version. Image blobs are cached across rebuilds.
Restart the development command after editing an external manifest file.

For a downloaded release manifest, pass its path as `MANIFEST` instead. Outside
watchnbuild, set `DISCOBOX_RELEASE_MANIFEST=/absolute/path/manifest.json` and run
`./build/discobox admin server --binary ./build/discobox-server`.

To exercise server downloads using the development CLI:

```sh
go tool task dev:released-server MANIFEST=releasemanifest/examples/v0.8.0.json
```

To stage downloads without starting a server:

```sh
DISCOBOX_RELEASE_MANIFEST=/absolute/path/release.json ./build/discobox admin server stage
```

To test automatic server download/start when no server is listening, set the
same environment variable and use `./build/discobox --auto-start-server=true run`.
`./build/discobox admin server manifest` prints the selected full release file.

The v0.8.0 example backfills the full metadata for a release predating this format.
Binary URLs, sizes, and SHA-256 digests come from the
[GitHub release asset metadata](https://api.github.com/repos/discobox-ai/discobox/releases/tags/v0.8.0),
and the source revision comes from the
[tagged commit](https://api.github.com/repos/discobox-ai/discobox/commits/v0.8.0).
Independent guest pins were checked against
[guestimage/image.go](https://github.com/discobox-ai/discobox/blob/v0.8.0/server/providers/guestimage/image.go)
and [libkrun/provider.go](https://github.com/discobox-ai/discobox/blob/v0.8.0/server/providers/libkrun/provider.go).
