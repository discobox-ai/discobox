# 26-11-19-780 — The Docker sandbox base ships Node and npm

**Status:** Proposed

**Date:** 2026-11-19

## Context

The [Docker sandbox base](../../server/providers/dockerworker/sandbox-image/Dockerfile)
excludes Node under [ADR 26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
§10. [ADR 26-11-06-664](26-11-06-664-common-apt-packages-are-built-into-the-docker-base-before-development-binaries.md)
kept that rule when it moved common apt packages into the image.

Every built-in harness except shell declares the `node` feature as a
dependency. On the stock Docker base, first-box provisioning runs
[`install-node.sh`](../../sandbox-agent/features/node/install-node.sh), which
configures NodeSource's 25.x apt repository and installs `nodejs`. That
network and package-manager work delays harness readiness on a fresh install
(#1399). The [feature's adoption check](../../sandbox-agent/features/node/provision.yaml)
already accepts system-wide `node` and `npm` with Node at least 22.19.

## Decision

Make Node a specific exception to the two ADRs' Docker base exclusions.
Build the stock image with an exact NodeSource `nodejs` package version
from the same 25.x line the feature installs today. Ship the package's
bundled npm and npx with it. Use a NodeSource signing key checked against a
pinned fingerprint and scoped to the build-time apt source. Remove that
source, key and fetched apt metadata in the same layer so the image adds no
NodeSource repository to a running box's apt configuration. Build for amd64
and arm64. Implementation will select the exact package version and verified
fingerprint. Update the pin explicitly when refreshing Node or moving the
fallback feature to a new release line.

Provisioning still checks the actual box. When adoption succeeds, skip
the feature's Node install step and run its ordinary integration steps.
The feature owns selection, command links, the `npm-global` helper and
its prefix, pnpm in that prefix, and the agent store. Keep harness
dependencies on the feature so they work with any usable provider Node.
Keep the same fallback install path for images that lack a usable pair.

The distribution change is confined to consumers of the stock Docker
sandbox base, including the Docker pool container that shares it. Native
exe.dev and boxd images and the pool VM guest retain their current Node
delivery. The browser, Nix, brew and coding agents remain feature-provided.

## Alternatives considered

- **Leave installation to provisioning and cache the download.** A cold
  host still needs its first download, source setup and apt installation.
  A shared image layer amortizes installation across its containers.
- **Bake Node alone.** Adoption requires a working npm too. Shipping them
  from one package preserves the pair the fallback already installs.
- **Bake pnpm or run the feature during the image build.** The current pnpm
  check expects the feature's selected npm and Discobox-owned prefix.
  Baking pnpm without that setup would leave the provisioning step in
  place. Baking the setup would couple the provider image to the feature's
  command selection and package management.

## Consequences

The shared image grows, and a cold host pays the additional layer pull.
Docker first-box provisioning still performs npm work for pnpm and harness
packages. The gain to measure is the net change in first-box readiness,
including the cold image pull.

Implementation evidence must report the compressed pull size and local
image size before and after, and compare time to first harness readiness
on equivalent cold hosts for the old and new image. The new image's
provision log must show adoption of Node without a NodeSource fetch or
`apt-get install nodejs` during provisioning. Exercise the fallback in
a test with an image lacking Node.
