# Release manifests

A manifest names runtime images by role: pool agent, sandbox agent, VM guest,
libkrun kernel, and harness slug. `Images.References` derives the host cache's
staging list from those roles. Never add a separately maintained preload list.
The VM/kernel retain their independent release pins; container image tags name
the Discobox release. Windows supplies its own guest through WSL Containers.

`servers` optionally carries the existing verified server-download descriptors,
one per platform. Keeping these optional lets local CLI/server builds consume
exactly the same image manifest without downloading a released executable.
The manifest version describes its artifacts, not the version of a local binary
using them. Explicit binary selection and a sibling development server retain
priority; otherwise the CLI can download its platform's server from `servers`.

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

`discobox-server manifest` exports the release image references compiled into
that server. Release builds produce a full
`discobox-manifest-<os>-<arch>.json` asset alongside the binaries, adding the
verified server download descriptor after signing the binary.

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

The v0.8.0 example backfills an image-only manifest for a release that predates
this file format. Its container references come from that release's image tags;
its independent guest pins were checked against
[guestimage/image.go](https://github.com/discobox-ai/discobox/blob/v0.8.0/server/providers/guestimage/image.go)
and [libkrun/provider.go](https://github.com/discobox-ai/discobox/blob/v0.8.0/server/providers/libkrun/provider.go).
