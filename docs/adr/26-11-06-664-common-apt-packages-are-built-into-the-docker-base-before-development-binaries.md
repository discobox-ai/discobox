# 26-11-06-664 — Common apt packages are built into the Docker base before development binaries

Status: Accepted
Date: 2026-11-06
Accepted by the owner by merging #755, 2026-11-06
Issue: [#740](https://github.com/discobox-ai/discobox/issues/740)

## Context

The issue's rc.4 release validation measured 210.9 seconds from the first
`discobox run --no-source -H claude-code` to the harness screen, and 71.5
seconds on a warm run. The base image was a 413 MiB download. Installing
packages separately during each box's provisioning adds network and apt work
after the image has already arrived.

The owner clarified the scope: install common apt packages while building the
existing Docker base in CI/release, and keep that work before frequently rebuilt
development binaries so their changes preserve the package-layer cache.

At `00541f9c90fefeabe1b61134a737fab287861803`:

- `server/providers/dockerworker/sandbox-image/Dockerfile` already installs
  Debian tools, Docker, gh, Go and uv. It copies no Discobox binaries. Both a
  Docker pool's container and its sandbox containers run this base.
- `sandbox-agent/features/*/provision.yaml` declares further apt packages:
  core needs SFTP support and process/account tools; build, cli, python and
  shells-editors add common development tools. Pool provisioning declares git,
  already present in the base.
- `sandbox-agent/provision/steps.go` currently updates apt indexes and runs
  apt install for a new packages step, even when the image already has all
  requested packages. Adding packages to the image alone would leave that
  network work at boot.
- `pool-agent/Dockerfile` and `sandbox-agent/Dockerfile` build Go programs
  and copy them into separate scratch bundles. Their source changes need not
  invalidate the independent base-image build.

## Decision

### 1. Install common Debian packages in the existing base-image build

Extend `server/providers/dockerworker/sandbox-image/Dockerfile`'s apt package
layer with the Debian package sets of core, build, cli, python and
shells-editors. Keep packages already present only once. This includes
`openssh-sftp-server`, `passwd`, `procps`, `patch`, the CLI utilities,
Python's pip/venv/requests support, and `screen`, `tmux`, `vim` and `zsh`.
Install dependencies normally with `--no-install-recommends`.

These packages are base software available to both pools and boxes. Feature
provisioning still owns configuration, wiring and readiness. Declaring a feature
off prevents its provisioning; it does not uninstall software supplied by the
provider image, consistent with the existing feature contract.

The feature declarations remain the portable requirements for other providers
and custom bases. The Dockerfile's package list is an explicit provider
optimization. Check it against those declarations when they change; do not
copy the complete feature tree or run the provisioning agent to construct
this apt layer.

The selected sets use Debian's existing repositories. Keep today's separate
Docker and gh repository layers. Node's NodeSource setup, npm/pnpm and harness
installation remain feature provisioning: this decision concerns ordinary apt
package prerequisites and does not preprovision a harness. Browser/desktop
packages are deferred because their larger dependency closure taxes every
pool and shell-only box; reconsider them if package-byte and boot-time
measurements show that trade worthwhile.

### 2. Package layers precede all frequently changing application inputs

In every final runtime-image ancestry, complete apt repository setup and
package installation before copying Discobox executables or application
source. Keep Go compilation in its builder stages and application binaries
in the later bundle/copy stages. The base remains independent of those
bundles; there is no need to merge their Dockerfiles.

Only distribution selection, package lists, repository keys/settings and
other actual package-install inputs may invalidate the apt layer. Do not
place `COPY . .`, a source-tree checksum, git revision, release version or
compiled binary before it. Copy any small package-specific input separately.
Changing a Go source file or rebuilding its executable must reuse the apt
layer even if the later binary/bundle layer changes.

Use the same layering in local development and CI/release builds. Docker's
layer cache is the primary reuse mechanism. Apt cache mounts may accelerate
an actual package-layer rebuild, but are not a substitute for correct layer
ordering. An explicit package refresh must remain possible for security
updates; caching must not mean that a package layer can never be rebuilt.

### 3. Provisioning skips satisfied apt package sets without network access

Before running apt for a typed `packages` step, check the local dpkg database
for every requested package. Only a fully installed package satisfies it;
missing or partially configured packages take the normal apt path. Preserve
any version/architecture requirements supported by the package declaration.

If all requirements are satisfied, skip both `apt-get update` and install,
and record the step as successfully satisfied through the normal runner.
Keep the apt/dpkg serialization and recheck under that lock before deciding.
Do not invent precompleted provisioning records in the Docker build: the
actual box checks its actual package database.

A matching step record must not conceal a subsequently removed package.
Check package satisfaction on subsequent provisioning too; repair missing
packages with the ordinary path. Other providers and custom bases continue
to install whatever they lack. A satisfied package set does not request an
upgrade or refresh package indexes on every boot. Package/security refresh
comes from an explicit refresh or a rebuilt base under existing upgrade rules.

Scripted apt steps retain their own checks and idempotence; do not globally
skip arbitrary scripts merely because some packages happen to be present.
Do not suppress failure of an apt update when installation is actually needed.

### 4. Publish and pin the image exactly as today

CI/release builds and publishes the larger base for the existing supported
architectures. The server's existing image pin names it, and current staging,
OCI cache, load-before-pull and image reclamation continue to apply. No new
release artifact, snapshot format, manifest schema, installer option or
background download service is required.

The download grows by the selected packages' compressed layers and their
missing dependencies. Docker's unpacked store grows by their installed size;
the staging cache may also keep the compressed blobs. Shared base layers are
fetched once per cache/daemon rather than once per container, and the software
would otherwise occupy per-container writable layers after provisioning.
There is no reliable size estimate yet: implementation must report the exact
before/after compressed image size, unpacked size and transferred bytes.

This is the default content of the published Docker base. A user who wants a
smaller image can use the existing provider `sandboxBaseImage` override, with
normal provisioning filling missing packages. No new opt-in/opt-out mechanism
is added. An explicitly selected full release manifest takes precedence over
image overrides (`EffectiveSandboxBaseImage`); custom-image users must use the
existing configuration path that permits that override. Feature opt-outs do
not remove packages already supplied by the base.

The base also serves Docker-backed VM pools, so their containers benefit
without altering the libkrun guest or WSL distribution. This decision changes
no native machine image. It preserves ADR 26-10-16-423 §10's exclusion of Node,
browsers, Nix, brew and coding agents from the Docker base, and
26-10-24-953's ordered bundle provisioning.

## Alternatives rejected

- **Only install at boot and rely on apt's download cache.** Each fresh box
  still resolves/installs packages. Building them into the base also reuses
  their installed files across containers.
- **Run the entire feature provisioner during the image build.** It couples
  stable apt work to frequently changing binaries, configuration and feature
  trees. The requested optimization needs only package layers and a local
  satisfaction check during provisioning.
- **Copy application binaries first and install packages afterward.** A Go
  rebuild invalidates every downstream apt layer, defeating development
  caching even when the package set is unchanged.
- **Installer prefetch, release seeds or provisioned snapshots.** Those solve
  different timing/transport problems and introduce lifecycle and compatibility
  decisions beyond this package-layer change. Existing staging and snapshots
  remain available without being extended here.

## Validation and consequences

Implementation updates the provider and provisioner design docs with the code.
The ADR itself changes no runtime behavior and claims no measured speedup.

Verify the package fast path on both a preloaded base and a custom base with
missing packages, including a removed package after a prior successful step.
Observe that a box and pool with satisfied package requirements make no apt
index/package downloads for those typed steps. Exercise other feature setup
normally so package availability is not mistaken for complete provisioning.

Build once, change/rebuild a Discobox Go executable, and build again with plain
BuildKit progress retained. The apt layers must report cache hits. Then change
a package-list input and show the apt layer rebuilds. Merely checking Dockerfile
instruction order does not prove the real build graph has the intended cache
behavior.

Release validation repeats the issue's cold and warm command/timing boundaries
on comparable hardware and network, records server-up, box-running, first
harness screen, total time and image/disk costs, and applies its 25% latency
rule. Record both fresh installation and upgrade. Include all image-transfer
time in the cold result. The package optimization must measurably reduce
provisioning without hiding an excessive cold-pull regression. Harness npm
installation remains, so a warm-equivalent first harness screen is a goal to
measure, not a promise of this narrower decision. If the issue's broader
latency goal is still unmet, record the remaining phase and raise that
follow-up explicitly rather than expanding this change into snapshotting.
