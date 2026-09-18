# Release manifests

A manifest names runtime images by role: pool agent, sandbox agent, VM guest,
libkrun kernel, and harness slug. `Images.References` derives the host cache's
staging list from those roles. Never add a separately maintained preload list.
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
remain user-owned, and existing sandboxes retain their pinned images.

The CLI can stage directly from the manifest without executing a development
server to discover images. Direct server startup stages the same image set
before initializing services, while its startup endpoint reports the download.
Pool provisioning subsequently imports cached container images before making a
pool available. Harness seeding reads metadata from the selected harness images and fails
startup if inspection fails. The manifest supplies the seed set even when the
development binary knows about additional harnesses.

`discobox-server manifest` exports the compiled image inventory. The release
build first signs and hashes the server and links that download descriptor into
the CLI. After the CLI's final link, it writes the full platform metadata to
`discobox-manifest-<os>-<arch>.json`. A CLI cannot embed its own final digest;
the full inventory is an external release artifact.

`release:manifest` merges all platform results into `release.json` before
publication. It rejects mismatched versions, source revisions, image sets,
duplicate platforms, and missing CLI/server pairs. The existing binary download
manifest remains the compact embedded server bootstrap descriptor.

## Development

Inside the Nix development shell:

```sh
go tool task dev:released-images MANIFEST=releasemanifest/examples/v0.8.0.json
```

In another terminal, use `./build/discobox new`. The development loop builds and
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
