# 26-11-23-787 — Every published asset carries GitHub build provenance, and a person verifies it by hand

- **Status**: Proposed
- **Date**: 2026-11-23
- **Issue**: #1571
- **Would supersede**: [0099](0099-the-cli-downloads-the-server-it-starts.md)
  §8's condition for revisiting signatures. That condition is met by §1 below,
  and §5 replaces it with a new one. §8's deferral of automatic verification
  stands, and so does the rest of 0099.
- **Relates to**: [0110](0110-the-install-script-is-a-release-asset-and-a-channel-names-which-one-runs.md)
  (stamped installer digests, and its rejection of a `SHA256SUMS` asset),
  [26-11-16-249](26-11-16-249-main-is-a-channel-built-into-its-own-repository-and-latest-is-no-channels-name.md)
  (main builds published in `discobox-ai/discobox-main`), and
  [0066](0066-the-build-is-nix-plus-taskfile-and-github-actions-only-triggers-it.md) §1
  (workflows hold no logic).

## Context

The installers check each binary they download against a SHA-256 written into
the installer itself (0110 §1). The CLI checks the server it stages against a
manifest linked into the CLI (0099 §3). Both chains are rooted in the first
thing fetched: the installer from `discobox.ai`, or the CLI from wherever it was
installed. That stops a bad mirror, since a binary from GitHub or
`assets.discobox.ai` has to match the stamp. It does not stop a modified
installer served by `discobox.ai` itself, because the stamp would describe the
modified binary. #1571 asks for a check that does not depend on `discobox.ai`.

At `97b22818`, no release signs or attests anything:

- `release:publish` (`Taskfile.yml`) uploads `build/release/bin/*`: the CLI and
  server for every platform, the winget archives, the per-platform
  `discobox-manifest-*.json`, `release.json`, and the stamped `install.sh` and
  `install.ps1`. The manifest and both installers are generated within
  `release:publish` (by `release:manifest` and `release:installers`), because
  only the publish job has every platform's binaries.
- The `publish` jobs in `.github/workflows/release.yml` and `main-build.yml`
  have no `id-token: write` and no `attestations: write`. The only
  `id-token: write` is on the `images` jobs, for Depot.
- macOS binaries get an ad-hoc `codesign --sign -` (`Taskfile.yml`, the
  entitlement step), which shows the file is intact but identifies nobody. No
  other platform is signed.
- `discobox-ai/discobox` is public, so GitHub artifact attestations cost
  nothing and are recorded in Sigstore's public-good transparency log.

0099 §8 deferred signatures "when there is a Developer ID or a Sigstore
identity to verify *against*". A GitHub Actions workflow's OIDC identity, as
Sigstore certifies it, is that identity.

## Decision

### 1. Every asset a release or main build publishes gets a GitHub build-provenance attestation

The `publish` job in `release.yml` and in `main-build.yml` attests every file
in `build/release/bin`, using `actions/attest-build-provenance`. That covers
every binary, both stamped installers, `release.json`, the per-platform
manifests and the winget archives. The attestation is SLSA build provenance,
signed with a short-lived Sigstore certificate for the workflow's identity, and
stored in GitHub's attestation store for `discobox-ai/discobox`. Each file is a
subject by its SHA-256, so the attestation covers exactly the bytes uploaded.

Only the `publish` job gets `id-token: write` and `attestations: write`. The
jobs that build binaries never hold a signing identity.

**Attest before upload.** Nothing is uploaded until its attestation exists. A
release must never be visible with unattested assets, and a main build must
never be marked latest without them. That means `release:publish` has to
stage its files (manifest, installers) before it uploads them, so that the
attest step can run in between. Implementation decides how to split the
Taskfile target. A failed attestation fails the job before the upload.

**Attesting is a workflow step, not a Taskfile target.** 0066 §1 keeps every
step in the Taskfile so it can run on a developer's machine. Attesting can't:
its input is the runner's OIDC token, which exists only in a GitHub Actions
job. Like `docker/login-action`, it is part of *where* a step runs. The
Taskfile still decides *what* gets published: the attest step's subjects are
whatever the Taskfile staged in `build/release/bin`, with no list kept in YAML.
This narrows 0066 §1 the same way the login step already does, and it is the
only exception this ADR adds.

### 2. One repository to verify against, and the workflow says which channel

A main build runs in `discobox-ai/discobox` and publishes to
`discobox-ai/discobox-main` (26-11-16-249 §4). The attestation is written by
the workflow's own `GITHUB_TOKEN`, so it lives with `discobox-ai/discobox`, and
its certificate names `main-build.yml`. So every channel verifies against the
same repository, and the signing workflow is what tells channels apart:

| Channel | `--repo` | `--signer-workflow` |
| --- | --- | --- |
| `stable`, `beta`, `edge`, a pinned tag | `discobox-ai/discobox` | `discobox-ai/discobox/.github/workflows/release.yml` |
| `main` | `discobox-ai/discobox` | `discobox-ai/discobox/.github/workflows/main-build.yml` |

The implementing change confirms both rows against a real release and a real
main build before it documents them.

### 3. The documented check is `gh attestation verify`, by hand

`docs/public/getting-started.md` gains a short "Verify a download" section with
two checks:

- **A binary already installed or downloaded:**
  `gh attestation verify <file> --repo discobox-ai/discobox --signer-workflow <row above>`.
- **The installer, before it runs.** Download `install.sh` (or `install.ps1`)
  to a file, verify it the same way, and then run it. The installer that
  `discobox.ai` serves is the stamped release asset byte for byte (0110 §4), so
  it verifies. A verified installer checks every binary against digests it
  carries, so the binaries are covered too. This check doesn't depend on
  `discobox.ai`, which is what #1571 asks for.

The docs give the `--signer-workflow` flag in every example. `--repo` alone
would accept an attestation from any workflow in the repository.

The docs also cover one limit: **a delegated installer is not covered.**
`--version` and `--channel` make a script download and run another release's
installer with no digest (0110 §2, §5). To verify a particular release, a
person downloads that release's `install.sh` from its release page, verifies
it, and runs it with no arguments.

Homebrew installs the same release assets, so a Homebrew user verifies the
installed binary the same way. Nothing changes in the tap.

### 4. Nothing verifies automatically, for now

- **The installers stay unchanged.** A POSIX `sh` script can't verify a
  Sigstore bundle without a verifier it would first have to download and trust.
  Requiring `gh` would break the installer's purpose, which is to work on a bare
  machine. The stamped digests remain the installers' check, and the attestation
  is an independent check on top of them, never a replacement.
- **The CLI does not verify the server's attestation when it stages it.** The
  digest the CLI carries already authenticates the server against the CLI the
  user chose to run (0099 §8). Checking a signature over the same bytes, signed
  by the same workflow that built that CLI, adds no trust to a manifest that is
  linked in. It would also cost every first stage a fetch of Sigstore's TUF
  root and GitHub's attestation API, and add an offline failure that 0099 has
  so far avoided.

### 5. 0099 §8's revisit condition is replaced

Revisit automatic verification when either of these happens:

- **Something is fetched that no trusted binary carries a digest for.** The
  first expected case is a manifest fetched at stage time (0099 §3's rejected
  alternative), or a delegated installer (§3 above). At that point a signature
  is the only thing that can authenticate it, and the verifier ships with the
  code that fetches it.
- **macOS or Windows binaries get a platform signing identity** (a Developer
  ID, Authenticode). The OS then verifies those binaries itself, and the
  question changes from "add a verifier" to "which identity does the CLI pin".

### 6. Images are out of scope, and deferred

The ghcr images `release:images` pushes (pool agent, sandbox base, bundles,
harnesses) are not attested by this ADR. The server pins them by tag
(`DefaultPoolImage`, `harness.BundleTag`), not by digest, so a person can't
verify "the image this server will pull" without first resolving a mutable tag.
An image attestation would also have to be made in the `images` job, which
runs in parallel with the binaries. Revisit when the server references its
images by digest, or when anything verifies an image before running it.

## Consequences

- Anyone can check a release asset against `discobox-ai/discobox`'s release
  workflow, with no trust in `discobox.ai`, the mirror, or the installer that
  delivered it. Tampering with an asset after upload, or serving a modified
  installer from the site, is detectable by anyone who runs the documented
  check.
- The trust root for that check is GitHub, meaning its Actions OIDC issuer and
  its attestation store, plus Sigstore's public-good instance. A compromise of
  the release workflow itself still produces valid attestations. Provenance
  records which workflow and commit built a file. It does not prove the commit
  is benign.
- The `publish` jobs gain `id-token: write` and `attestations: write`. The
  attest step adds one API call per subject set, which takes seconds.
- Releases cut before this lands have no attestations, and nothing backfills
  them. The docs say which release is the first with attestations.
- A fork's release workflow attests into the fork, under the fork's identity.
  That is correct: its builds are not `discobox-ai/discobox`'s.

## Open, for implementation

- **How `release:publish` splits** so the attest step can run between staging
  and upload (§1). Whatever the split, it keeps the rule that no asset is
  uploaded unattested.
- **Whether the docs also pin the source ref** (`--source-ref` or a
  certificate-identity regex) for tagged releases. `release.yml`'s
  `workflow_dispatch` runs off a branch, so the signing ref isn't always the
  tag. The implementing change checks what a dispatched release's certificate
  names before it documents a ref.

## Rejected alternatives

**A cosign-signed `SHA256SUMS` asset.** It is portable: `cosign verify-blob`
needs no GitHub API. Signed keylessly, its identity is the same GitHub OIDC
identity that §1 attests with, so it adds no trust. It adds a file that only
exists to be signed. It also splits each asset's URL and digest into separate
places, which 0110 rejected for an unsigned sums file. Signing it with a
long-lived key instead means taking on key custody, rotation and revocation
that nobody owns today.

**Sigstore bundles uploaded as release assets** (`<asset>.sigstore.json`).
They would allow verification with no call to GitHub's attestation API. But
they double the asset count, nothing in the installers or the mirror reads
them, and anyone who needs a bundle offline can get it with
`gh attestation download`.

**GPG or minisign signatures.** These have the same key-custody cost as a keyed
cosign, and a verifier that most users don't have installed.

**Verify in the installers.** This was rejected in §4: a bare-machine script
can't carry a Sigstore verifier, and making it depend on `gh` breaks what the
installer is for.

**Verify in the CLI at stage time now.** This was rejected in §4: it adds no
trust while the manifest is linked into the CLI, and it costs network and
offline robustness. §5 says when that changes.

**Attest in each `binaries` matrix job.** Each runner would attest the files it
built. But the installers and `release.json` only exist in `publish`, so
`publish` would have to attest anyway. Giving every builder an identity
token also widens what can sign.
