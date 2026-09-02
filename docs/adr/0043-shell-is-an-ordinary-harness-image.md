# 0043 — `shell` is an ordinary harness image

- **Status**: Accepted (§2, an omitted `runCommand` meaning the login shell, is
  superseded by [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md);
  §1 and §3 stand)
- **Date**: 2026-08-15
- **Supersedes**: [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md) §2 — `shell` as a seeded built-in whose image is the default sandbox image. §1 (every sandbox carries a harness config) and §3 (the chain ends at the reserved slug) stand unchanged.

## Context

`shell` was not an image. It was the sandbox agent image wearing a harness
config, appended to the seed list rather than registered, on the reasoning that
the registry holds harness *products* built on top of the agent and this one
*is* the agent.

That one exception spread. Its image had no `io.discobox.image.v1` label,
because a label had to declare a harness and the agent is not one — so it could
not be inspected, so its digest had to be configured out of band
(`DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST`) and threaded through `Seed.Digest`,
`harnessconfigs.SetDefaultSandboxImage`, and a branch in `SeedBuiltIns` that
skipped inspection entirely. `Configured` was set by comparing against the
reserved slug. Four distinct mechanisms existed to say "this one is different".

The cost surfaced when the shell harness needed `additionalGroups: ["docker"]`,
which the two harness images already declare. The agent image ships the Docker
CLI, and that CLI checks group membership rather than relying on the sudo access
every sandbox user has — so without the group, Docker works as the sandbox user
under a coding harness and not under a plain shell. There was nowhere to declare
it: no `image.json`, no label, nothing to inspect. Every available answer was
another exception — state it in Go beside the seed, or teach the base image to
carry a harness-less label that only this one path reads.

An exception that has no place to put a one-line declaration is not carrying its
weight.

## Decision

### 1. `shell` is a harness image like any other

`harness/shell` joins `harness/claude-code` and `harness/codex-cli`: its own
folder, `Dockerfile`, `image.json`, and driver, built `FROM` the sandbox agent
base through `SANDBOX_AGENT_IMAGE`, registered in `harness/registry`, seeded and
inspected exactly like its siblings. Its `Dockerfile` installs nothing — the
harness it provides is the shell the base image already ships — so the image is
its declarations and one empty layer.

The cost is one more image built, published, and pulled per release for no
additional software. That is the price of the exception being gone, and it is
worth paying: every mechanism listed above is deleted, not relocated.

The reserved slug stays. `shell` is still the end of the harness resolution
chain, and nothing else may claim that name.

### 2. An omitted `runCommand` means the login shell

The image contract makes `runCommand` optional, and its absence is a
declaration: *the sandbox resolves the user's login shell*. This is not new
behavior — `sandbox-agent`'s terminal layer already treats a declared harness
with no command that way — but it was previously reachable only by a config the
image contract refused to describe.

It is the same rule as ADR 0025 §3: which shell a user has is sandbox knowledge,
because the account lives in the image's passwd database, which does not exist
until the container does. A harness image declaring `["bash"]` would be stating
something it cannot know, and would silently override a user's `zsh` or `fish`.

A *blank* command is still rejected. "Declares nothing" and "declares an empty
string" are different, and only one of them is a decision.

### 3. `Configured` is derived from what the image collects

A built-in is seeded configured when its image declares no secrets, rather than
when its slug is `shell`. A harness with no credentials to collect is ready the
moment it is seeded, which is what a fresh project needs in order to be usable
before anyone configures anything. Reseeding still never revisits the flag.

## Consequences

- `Seed.Digest`, `harnessconfigs.SetDefaultSandboxImage`, the digest-carrying
  branch in `SeedBuiltIns`, and the slug comparison behind `Configured` are all
  removed. `SeedBuiltIns` inspects every seed and skips the ones it cannot read,
  with no special cases.
- The server's default sandbox image keeps its remaining job: the image a
  sandbox runs when it has no harness config at all
  (`resources/sandboxes/service.go`). It no longer seeds anything.
- Agent upgrades still reach a shell sandbox, which is what ADR 0032 §2 bought
  by pointing `shell` at the agent image directly. The shell image is rebuilt on
  each new agent base like every other harness image, so its digest moves and
  the existing upgrade comparison works unchanged.
- A deployment must publish a shell image and point `DISCOBOX_HARNESS_SHELL_IMAGE`
  at it exactly as it does for the other harnesses. If it does not, `shell` is
  skipped at seeding like any unavailable harness, and a sandbox created with no
  harness config falls back to the server's default sandbox image — the same
  path that already handles an uninspectable built-in.
- A shell sandbox now gets the volumes its siblings declare — a persistent
  `$HOME`, and `/var/lib/docker` and `/var/lib/containerd` on the `data` volume,
  without which nested Docker fails on overlay-on-overlay. It previously
  declared none, so it had neither.
- Registering a user image whose label omits `runCommand` now succeeds and runs
  that sandbox's login shell, where it was a 400 before. This follows from §2
  and is not a separate decision.

## Alternatives considered

**Declare the group in Go, beside the seed.** Smallest diff, and it reaches
existing deployments without rebuilding an image — the declaration is compiled
into the server, so an unchanged image digest still picks it up. Rejected
because image-owned data would live in the control plane, and the digest would
stop being a complete freshness key for what an image declares.

**Give the sandbox agent base its own harness-less label.** Keeps the
declaration in the image and adds no new image. Rejected because it makes the
base image simultaneously the thing harnesses are built on *and* a thing the
harness-config seeder reads, which is the same conflation this ADR removes —
and it still leaves `shell` with a configured digest, since the base image's
identity is the one the server is configured with.

**Declare `runCommand: ["bash"]` and drop §2.** Makes `shell` completely
ordinary with no contract change. Rejected because it hardcodes one shell for
every user, overriding the account's own — the exact thing ADR 0025 §3 resolved
by putting shell resolution inside the sandbox.
