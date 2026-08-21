# 0066. The build is Nix plus the Taskfile, and GitHub Actions only triggers it

Status: Accepted

Date: 2026-08-20

## Context

Every check and the entire release ran through Dagger. `dagger.toml` declared a
workspace whose `go` module ran lint and tests in containers, and
`.dagger/modules/release` was a Dang program that staged and published the
release. The two GitHub workflows did nothing but install the Dagger CLI and
call it.

That arrangement had three problems, and the third is the one that matters.

**It rotted without anyone noticing.** The release module built images from
`worker-agent/Dockerfile`, a path that stopped existing at the pool rename
(`fa3841e8`). Every push to `main` ran a release dry run that could only fail.
By the time `vz` landed, the same module's `CGO_ENABLED=0` cross-compile had
broken darwin as well, because `Code-Hex/vz` is cgo. Neither was caught, because
nobody runs a release dry run locally and CI's failure was ambient.

**It was a second language and a second toolchain for work the Taskfile already
described.** `task check`, `task test:all`, and `task generate` exist and are
what a developer actually runs. The Dagger workspace re-expressed a subset of
them in `dagger.toml` settings and Dang, so the two could — and did — drift.

**It could not be run the way it runs in CI.** `dagger check` in a terminal and
`dagger check` in Actions were the same command, which is the property Dagger
sells, but the thing being invoked was opaque: a container graph assembled by a
beta engine, not the commands a developer debugs with. When it failed, the
reproduction was "run the pipeline again."

Meanwhile the repository already had two answers to the same questions. `mise`
pinned Go and Task, deriving both versions from `go.mod` via
`scripts/mise-version`. `flake.nix` pinned the system-level toolchain the
libkrun backend needs — `docker-buildx`, `e2fsprogs`, `qemu-utils`, `passt`,
Rust — and ADR 0013 §2 already established the flake as the owner of build
tooling. Two tools pinning overlapping sets of things, neither covering the
whole.

And the platform matrix stopped being cross-compilable. `vz` made darwin a cgo
target that needs the macOS SDK and, because creating a VM requires
`com.apple.security.virtualization`, a codesigning step that only exists on
macOS (ADR 0062 §1). A single Linux container cannot produce the release any
more.

## Decision

### 1. Three layers, and the top one holds no logic

- `flake.nix` owns system-level tools and environment.
- `Taskfile.yml` owns every step of check, test, build, and release.
- `.github/workflows/` owns *when* a step runs, on which runner, and how
  artifacts move between jobs. Nothing else.

Every job body is one line: `nix develop -c go tool task <target>`. A workflow
that needs a new check gets a Taskfile target and four lines of YAML, and the
same target runs in a terminal.

This is the property the Dagger workspace was bought for, obtained instead by
keeping the unit of work a shell command a developer already runs. The cost is
that GitHub's runner environment is no longer reproduced locally — only the
steps are. That trade is deliberate: reproducing a *step* is what fails in
practice, and reproducing a *runner* is what containers were doing expensively.

### 2. One composite action carries the repetition

`.github/actions/task` checks out, installs Nix, restores caches, and runs one
target. Every job across every workflow uses it and differs only by `target` and
`runs-on`. The alternative — a reusable workflow — was rejected because it
inverts the dependency: the caller supplies triggers and the callee supplies
jobs, which puts job structure back in YAML.

### 3. Nix replaces mise, and pins only a bootstrap Go

`mise.toml` and `scripts/mise-version` are removed. The flake's devShell gains
`go`, `nodejs`, `pnpm`, `gh`, `jq`, `bats`, `shellcheck`, and `git`; its
`shellHook` absorbs the `DISCOBOX_*` environment and completion generation that
`mise.toml` provided. `devShells` fan out to `x86_64-linux`, `aarch64-linux`,
and `aarch64-darwin`; `packages`, `apps`, and `checks` stay x86_64-linux, since
the libkrun artifacts are Linux-only.

`task`, `golangci-lint`, and `ogen` stay `go tool` dependencies in `go.mod`.
Nix does not pin them, because `go.mod` already does and two pins drift.

The Go toolchain is pinned by `GOTOOLCHAIN=auto`: Nix supplies a bootstrap Go
and Go fetches the exact version `go.mod` names. mise derived that version from
`go.mod` by script; nixpkgs cannot, and pinning nixpkgs to a revision whose `go`
happens to match go.mod is a coupling that silently goes stale. The cost is that
a cold build needs the network to fetch a toolchain.

The general shell holds no libkrun. `libkrun.override { withBlk; withNet; }` is
not the derivation `cache.nixos.org` has, so keeping it in the default shell
would make every CI job build libkrun from source for a toolchain none of them
use. `devShells.libkrun` — x86_64-linux only — keeps Rust, the overridden
libkrun, `passt`, `pkg-config`, and `qemu-utils` for launcher work, and
`nix build .#discobox-krun` needs neither shell.

On darwin the devShell is `mkShellNoCC`. `mkShell` injects Nix's clang, and the
darwin build must compile Objective-C against Virtualization.framework and sign
with `/usr/bin/codesign`. Deferring to the system Xcode toolchain on the one
platform where Apple owns the SDK is cheaper than reproducing it in nixpkgs.

### 4. Builds fan out natively; Windows is cross-compiled anyway

Release binaries are built on the platform they target, because darwin has to
be: cgo, the macOS SDK, and codesigning are all native-only.

Windows is the exception in both directions. Nix does not run on Windows, so
Windows jobs use `actions/setup-go` directly — the "do whatever works" lane. And
`wslc` carries no build tags and no cgo (it drives WSL by shelling out), so the
whole tree cross-compiles to `windows/amd64` with `CGO_ENABLED=0`, verified by
`go vet ./...`, which type-checks the test tree too. The Windows *binary* is
therefore cross-compiled from the Linux job.

A native Windows job still runs `go test ./...`, because compiling is not
running: the named-pipe transport, `wslc`'s process handling, and the ConPTY
implementation ADR 0065 §4 adds are Windows-only code paths that no other runner
executes. ADR 0065 keeps ConPTY free of cgo and of a new module, so this stays a
test job rather than a second build environment.

`darwin/amd64` is not shipped. ADR 0062 defers Intel Macs and
`assemble-guest-image` refuses non-arm64, so an Intel binary would install and
then fail at the first pool. Offering nothing is a better failure than offering
a binary that cannot do the thing it was installed for.

### 5. The entitlement is applied at build time, never at packaging time

`sign:server` becomes a general `sign` target taking a binary, and `build:cli`
chains it. This closes a gap the `vz` work left: `disco` is a single binary that
runs the server in-process (`cli/internal/cli/root.go:284`) and autolaunches
itself in release builds, so `disco` — not `discobox-server` — is the process
that creates the VM, and it was the one binary nothing signed.

Release darwin binaries are signed on the macOS runner. The Homebrew formula
does nothing but install.

Signing in the formula's `post_install` was considered and rejected. It is not a
privilege concern — ad-hoc signing is not an authorization mechanism, needs no
elevation, and `com.apple.security.virtualization` is honored on an ad-hoc
signature precisely because Apple does not gate it (unlike
`com.apple.vm.networking`, which `vz.entitlements` already refuses). It is an
integrity concern: it would mutate the binary after Homebrew verified its
checksum, and it would silently strip a Developer ID signature and invalidate
notarization the day one is added. `server/providers/vz/DESIGN.md:41` already
states the rule — codesigning is part of the build, not of packaging — and
post_install is packaging.

Entitlements live inside the Mach-O, in the signature blob at the end of
`__LINKEDIT`, so copying, `tar`, and `install` preserve them. Only a rewrite of
the binary's bytes can lose them, and Homebrew's bottle relocation both skips
binaries without cellar paths and passes `--preserve-metadata=entitlements` when
it does re-sign.

The formula carries a `caveats` block naming the entitlement and stating that
`com.apple.vm.networking` is deliberately absent, so guests are NAT'd rather
than on the user's LAN. A user who later inspects the binary should find nothing
they were not told about.

### 6. Images build with `depot build`, falling back to `docker buildx`

Release images are built with `depot build --platform linux/amd64,linux/arm64`,
which builds each architecture natively and removes both the per-arch fan-out
and the manifest-list assembly.

When the `depot` CLI is absent the task falls back to
`docker buildx build --platform ...`, which is correct but emulated. The task
sets up what buildx silently needs — a `docker-container` driver builder and
registered binfmt handlers — and warns about the cost, which is uneven: both
Dockerfiles already run their Go stage with `FROM --platform=$BUILDPLATFORM` and
`GOOS=$TARGETOS`, so no Go compilation is emulated. Only the Debian runtime
stages are, and `sandbox-agent`'s is roughly three hundred lines of apt plus a
code-server install. `pool-agent`'s is near-free.

### 7. Unit tests do not depend on locally built harness images

`server/internal/service` currently fails on any machine without the built-in
harness images: seeding inspects `discobox-harness-{codex,claude-code,shell}`
and skips what it cannot find (`harnessconfigs/service.go:388`), and fifteen
tests then fail with "harness config not found". A fresh runner is such a
machine.

CI builds three label-only stub images — seeding needs nothing but an
inspectable `io.discobox.image.v1` label — and points
`DISCOBOX_HARNESS_<SLUG>_IMAGE` at them. That override already exists
(`harnessdefs.ImageEnvVar`).

Building the real images in CI was rejected: it is the sandbox-agent base plus
three harness images on every pull request, for a suite that needs a label.
Rewriting the fifteen tests to construct configs directly is the better end
state and remains open, but it is a change to test code that the build work does
not need to block on.

## Consequences

`dagger.toml`, `dagger.lock`, `.dagger/`, `mise.toml`, and
`scripts/mise-version` are removed, along with the Dagger skills.

The release stops being reproducible from a single machine. Producing a full
release requires a macOS host for the darwin half; locally, `task release:build`
builds the current platform and `task release:publish` consumes whatever
populated `build/release/`, so every step remains individually runnable, but the
fan-out is GitHub's.

Generated-file staleness needs its own target. Dagger's generators-as-checks
caught it; `generate:verify` — run `task generate`, then `git diff --exit-code` —
replaces it, alongside `fmt:verify` and `tidy:verify`.

The guest image workflow keeps its triggers but moves its body into `guest:*`
Taskfile targets, so its artifact verification is runnable locally.

This supersedes ADR 0062 §3's description of the release as staging
`worker-agent` and `sandbox-agent` from `.dagger/modules/release`. The guest
image's separate release line, which is what that section decided, is unchanged.
