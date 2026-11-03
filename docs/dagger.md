# CI and release with Dagger

Dagger owns the portable pipeline. GitHub Actions supplies events, credentials,
and conventional native macOS/Windows jobs;
Taskfile targets are shortcuts to the same local commands. Host development
(`task dev`, development binaries, and the image watcher) remains in Task.

Install **Dagger v1.0.0-beta.21** using the [official installer](https://docs.dagger.io/getting-started/install/)
and run a container runtime. Go, Node, pnpm, PowerShell, shellcheck, and build
utilities are installed inside the pipeline's toolchain image. A local Nix
shell is useful for host development but is not required for pipeline execution.
The Go SDK itself currently targets Go 1.26.10; the application toolchain is
Go 1.27.1. SDK-generated files and image/reference resolutions are committed.
Actions uses the official `dagger/dagger-for-github@v8.4.0` action to install
the same pinned CLI, then runs the commands below.

## Local checks

```sh
dagger check --list
dagger check
dagger api call ci-artifacts export --path=build/ci
```

`dagger check` runs the registered `pipeline/ci` check: workspace Go tests,
lint across all workspace modules, Windows cross-typechecking, cross-platform
deadcode, shellcheck, Dockerfile source/import coverage, formatting, standalone
module tidy, Go/Node generators, Mermaid, pipeline unit tests, Linux/Windows
release binaries for amd64/arm64, image trust/HTTPS validation, and revision-bound
native helper inputs. The Go SDK's generated-binding staleness check also runs.
Component functions remain available for focused runs; they are not separately
registered checks, so automatic Cloud Checks gets the same complete CI graph.

`ci-artifacts` exposes the portable outputs for export and native-job transport.
It materializes builds and trust validation but does not substitute for the full
`ci` check's tests/lint/verification. Actions runs `dagger check` before exporting
these artifacts. Modules are read from `go.work`, so adding one does not require
editing a duplicated pipeline list.

Useful individual entry points:

```sh
dagger api call tests
dagger api call static
dagger api call verify
dagger api call module-tests
dagger api call macos contents
dagger api call windows contents
```

Checks run on source snapshots and do not mutate the checkout. For edits:

```sh
dagger api call format -y
dagger api call tidy -y
dagger api call generate -y
dagger generate --module=go-sdk -y
```

The first three apply workspace Changesets. The last regenerates the pipeline's
own SDK bindings. Dagger never invokes Task, so these operations are also safe
behind file-change hooks that invoke the Task shortcuts.

## Dagger Cloud execution and cache

Trusted Actions jobs execute Dagger on Cloud Engines with `DAGGER_ENGINE=cloud`.
They authenticate through GitHub OIDC: the job grants `id-token: write` and
sets `DAGGER_CLOUD_TOKEN` to the literal **`oidc`**, not a repository secret.
The pinned CLI requests GitHub's short-lived token with audience `dagger.cloud`
and exchanges it with Dagger Cloud. The repository must be authorized in the
intended Dagger Cloud organization's GitHub integration before this exchange
can succeed. No static Cloud token needs to be stored in Actions.

The official action installs the pinned CLI. Engine and OIDC settings live in
the job environment because the workflows invoke Dagger in separate steps;
the action's `cloud-token` input only applies to commands that action runs itself.
Cloud Engines retain build cache beyond an ephemeral Actions runner. Native
macOS/Windows jobs still run directly on their own runners and receive neither
the Cloud token setting nor permission to mint OIDC tokens.

Fork and Dependabot PRs explicitly run the same graph on a local engine, with
Cloud authentication disabled. Actions invokes the same complete check before transporting native inputs.
Cloud Checks, when enabled for the repository, discovers this check and can run
it directly without an Actions runner. Release, preview, promotion and
publication remain explicit pipeline functions and are never registered checks.

Local execution remains the default. To use the same Cloud organization locally:

```sh
dagger cloud login
dagger --engine=cloud check
dagger --engine=cloud api call ci-artifacts export --path=build/ci
```

GitHub OIDC is available only inside Actions. Local/headless clients need their
own login or a CI token supplied through a credential manager. To return to the
local engine, omit `--engine=cloud` and unset any `DAGGER_ENGINE` override.
Never commit a Cloud token.

## End-to-end release rehearsal

```sh
dagger api call preview --version=v0.0.0-preview export --path=build/release/preview
dagger api call images-build export --path=build/release/oci
# Combine binaries and real OCI builds in one exported preview:
dagger api call preview --version=v0.0.0-preview --images export --path=build/release/full-preview
```

These commands perform real cross-builds and produce real binaries, embedded
server manifests, Windows version resources, ZIPs, per-platform manifests, and
build receipts, a merged manifest, partial-platform installers, and verified
SHA256SUMS. The preview installers only describe platforms actually built. Image builds produce OCI archives for the sandbox base, pool,
agent, platform, and all seven harness bundles. Pass `--platform=linux/arm64`
to `images-build` to exercise the other architecture. Neutral harness bundles
remain single manifests; other products publish multi-platform indexes.

The preview's `preview.json` explains the publication order and why preview
never publishes. Without Mac inputs, it honestly reports missing Darwin output.
`native/macos.json` and `native/windows.json` describe external commands; they
are not native test results.

## Native macOS and Windows

CI has three required jobs: `pipeline`, `native-macos`, and `native-windows`.
The native jobs follow the portable job: Mac downloads only the portable helper
inputs; Windows downloads and smokes the exact amd64 executables built by
Dagger. This avoids spending native runner time cross-building portable inputs.

On an ARM64 Mac with the Apple SDK, codesign, and the pinned application Go
version (from `go.mod`):

```sh
# Export portable helpers through the Linux Dagger toolchain; copy this
# directory to the Mac if it was exported on another machine:
dagger api call native-inputs export --path=build/native-inputs
# Darwin-specific package tests and lint/vet, native build/sign/smoke:
go tool task native:macos
# For a clean checkout of an existing release tag:
go tool task native:macos:build -- -version vX.Y.Z
```

On Windows with the same Go version:

```sh
go tool task native:windows
# Copy Dagger's Windows amd64 executables into build/native, then:
go tool task native:smoke
```

Actions invokes `go run ./internal/cmd/discobox-native-ci` directly; Task is a
local shortcut. Windows needs no Docker or Nix. Windows runs workspace tests. Mac tests packages whose Go/cgo/test files
differ from Linux plus the explicit native behavior contract in
`discobox-native-ci` (installer, clipboard, TUI, VZ, sandbox manager and service).
The complete Linux suite stays in Dagger. Add native runtime branches and
native dependency boundaries to that contract even when filenames stay the same. Mac also uses the existing Darwin-specific linter script and
builds with cgo and the Apple SDK. Portable helper receipts must match source
revision and repository, and each helper is hashed before it is embedded.
Tagged builds also reject helpers exported from a dirty source snapshot.

Mac signs and verifies the final server with the Virtualization entitlement
**before** computing its digest and linking the CLI. It then signs/verifies the
CLI, runs version/embedded-manifest smoke checks, and hashes the final files
into `receipts/darwin-arm64.json`. This preserves existing ad hoc signing;
Developer ID signing/notarization is not introduced.

Copy `build/native` from that Mac to the Linux release machine (or let Actions
transport it). Dagger consumes only the named Mac files, requires source,
repository, version, signing/smoke checks and matching final hashes, and restores
executable permissions lost by Actions ZIP transport. A complete nonpublishing
rehearsal is:

```sh
dagger api call preview --version=vX.Y.Z --native-artifacts=build/native export --path=build/release/preview
```

Native checks have to run on those operating systems; a green Linux job does
not establish their success. Missing Mac artifacts still prevent publication.
When targeting a fork, pass the same `--repository=OWNER/REPO` to Dagger and
`-repository OWNER/REPO` to the native command.

## VM artifacts

The guest and libkrun kernel retain independent pipelines and version lines:

```sh
dagger api call vm-build --kind=guest --platform=linux/amd64 export --path=build/vm-amd64
dagger api call vm-build --kind=guest --platform=linux/arm64 export --path=build/vm-arm64
dagger api call vm-build --kind=kernel export --path=build/vm-kernel
dagger api call vm-check --kind=guest
dagger api call vm-check --kind=kernel
```

These validate the actual root filesystem, kernel format/config, and boot
configuration. They do not claim to boot a VM. Foreign-platform runtime builds register binfmt using the same trusted
helper as the former buildx path, via privileged engine execution. Native
engines avoid that setup and emulation cost. The kernel build is substantially more expensive than ordinary CI.

## Publication and channels

All credentials are explicit Dagger Secret arguments. For example,
`--token=env:GH_TOKEN` reads an environment variable; `--token=cmd:'gh auth token'`
uses an existing local CLI credential. Nothing requires Actions' ephemeral
`GITHUB_TOKEN`, Depot, or Dagger Cloud. GitHub and the registry remain external
publication destinations.

```sh
# Build locally, without publication:
dagger api call release-build --version=vX.Y.Z export --path=build/release/bin
# This intentionally refuses incomplete native results:
dagger api call release-assemble --version=vX.Y.Z --artifacts=build/release/bin export --path=build/release/assembled
# Full production release with the matching Mac artifacts:
dagger api call release --version=vX.Y.Z --native-artifacts=build/native --username=USER --registry-password=env:REGISTRY_PASSWORD --token=env:GH_TOKEN --tap-token=env:TAP_TOKEN
# Independent VM release:
dagger api call vm-publish --kind=guest --version=vX.Y.Z --registry=ghcr.io/OWNER --username=USER --password=env:REGISTRY_PASSWORD
# Deliberate stable promotion of an existing release:
dagger api call promote --version=vX.Y.Z --token=env:GH_TOKEN --tap-token=env:TAP_TOKEN
```

The full release validates the source tag and clean checkout, runs CI and
builds, requires matching receipts for every release platform, assembles
`release.json`, stamps installers, hashes final assets, publishes versioned OCI
artifacts, uploads a GitHub prerelease, and then updates mutable channels for a
dot version. Missing native artifacts fail before any production publication.
No install script or formula is stamped with invented macOS digests.

Dot releases update OCI `latest` and Homebrew `discobox-dev`. Explicit semver
prereleases update neither. Stable promotion is separate and only accepts a
dot version. Homebrew updates both formulae directly; `TAP_TOKEN` therefore
needs contents write access to `discobox-ai/homebrew-tap`, rather than only
permission to dispatch its old workflow. No cross-repository Actions run is
needed. Promotion reads the existing release state and uses the current pipeline;
it does not require rebuilding or checking out the historical release source.

winget remains manual. `winget-manifests` downloads and checks the published
Windows archives; `winget-publish` opens or updates the community repository's
pull request with a classic public-repository PAT. Neither release nor promotion
submits winget automatically, and Microsoft still controls validation and merge.

Publication functions exist but running the local preview does not exercise
writes to production destinations. Runtime-dependent Go test skips are visible limitations. GitHub branch
protection must require `pipeline`, `native-macos`, and `native-windows`.

For the existing manual channel-override use case, `brew-publish` writes one
formula (`--dev` selects `discobox-dev`). Both `brew-publish` and
`winget-publish` require `--allow-prerelease` to publish an explicit prerelease
to a package channel. Automatic release and promotion never set that flag.
Formula and manifest generation alone do not publish anything.

The [migration parity audit](dagger-parity.md) maps the original CI/release
operations to Dagger, records same-source comparisons and isolated publication
tests, and records the original stubbed snapshot separately from later native integration.

The image trust path is checked with `go tool task ci:trust` (also part of
`dagger check`). It builds the existing base and platform Dockerfiles, runs core's
existing certificate-store script, verifies the saved certificates match the
image's own distro inputs exactly, and tests runtime CA addition with real
HTTPS. Results are exported as `build/ci/trust.json`. No host/toolchain CA
bundle is copied into the image.
