# 26-11-24-185 — Every published Windows executable is Authenticode-signed with Azure Trusted Signing, as it is built

- **Status**: Accepted
- **Date**: 2026-11-24
  Accepted by the owner by merging #1671, 2026-11-25.
- **Amended**: 2026-11-25, §3, §4 and §5, during implementation and before
  anything shipped against it. §4: a published build with no signing
  configuration ships unsigned with a warning, the owner's choice while the
  Azure account does not exist yet. §5: the expected subject is a repository
  variable. §3: the Linux leg is its own `binaries-linux` job.
- **Issue**: #1422
- **Relates to**: [0066](0066-the-build-is-nix-plus-taskfile-and-github-actions-only-triggers-it.md)
  §1 (workflows hold no logic) and §5 (signing is part of the build, not of
  packaging), [0099](0099-the-cli-downloads-the-server-it-starts.md) (the CLI
  stages the server by digest), [0110](0110-the-install-script-is-a-release-asset-and-a-channel-names-which-one-runs.md)
  (stamped installer digests), [26-11-16-249](26-11-16-249-main-is-a-channel-built-into-its-own-repository-and-latest-is-no-channels-name.md)
  (main builds), and [26-11-23-787](26-11-23-787-every-published-asset-carries-github-build-provenance-and-a-person-verifies-it-by-hand.md)
  (Proposed: build provenance). This ADR answers 787 §5's second trigger for
  Windows (§6 below), and narrows 787 §1's "builders never hold a signing
  identity" for an Authenticode identity alone (§3).

## Context

On Windows 11 with Smart App Control (SAC) on, `install.ps1` downloads
`discobox-windows-amd64.exe`, its digest matches, and running it fails with
"An Application Control policy has blocked this file". CodeIntegrity events
3033/3077 name SAC's policy, `VerifiedAndReputableDesktop`.
`Get-AuthenticodeSignature` reports `NotSigned`. Turning SAC off fixes it, and
the staged server then runs too. SAC is on by default on clean Windows 11
installs that pass its evaluation. It allows an unsigned file only if
Microsoft's cloud reputation already knows that file, and a release every few
days never builds that reputation. The fix is a signature.

At `e6f5ea44`, nothing signs a Windows executable:

- `release:build:linux` (`Taskfile.yml`) cross-compiles the CLI and the server
  for `windows/{amd64,arm64}` through `release:binary`.
  `discobox-winres` links a version resource, and nothing else is added.
  `release:windows-zip` then zips each CLI as `discobox.exe` for winget.
- **There are three Windows executables, not two.** `build:clipwatch:arch`
  builds `discobox-clipwatch.windows-<arch>.exe` and gzips it into
  `cli/internal/clipwatch/artifacts/`. The Linux CLI embeds it, writes it out
  under WSL, and runs it on the Windows host, where SAC applies to it the same
  way.
- **Order matters.** `release:binary` builds the server first, takes its
  digest into the manifest the CLI is linked with (`serverstage.DefaultManifest`),
  and then links the CLI. After that, `discobox-server-manifest -client-asset`
  writes the CLI's digest into `discobox-manifest-<os>-<arch>.json`.
  `release:publish` later stamps each binary's digest into `install.sh` and
  `install.ps1`. Signing rewrites a PE file, so every one of those digests has
  to be taken from the signed bytes.
- The only signing step is the macOS ad-hoc `codesign --sign -` in the
  `sign` target. 0066 §5 puts it in the build for a reason: signing after the
  checksum is taken breaks the checksum.
- The `binaries` jobs in `release.yml` and `main-build.yml` run
  `release:build` on a Linux runner and a macOS runner. `ci.yml`'s `build` job
  runs the same target on every push, with no tag.
- `pkg/installer/install.ps1` turns any failure to run into "installed …, but
  it does not run: …" and leaves the binary in place.
- #901 (desktop app signing) was closed without being built. No Windows
  signing credential exists.

## Decision

### 1. The signer is Azure Trusted Signing, logged in to by OIDC

Windows executables are signed through **Azure Trusted Signing** (renamed
Azure Artifact Signing), with a public-trust certificate profile. The owner
holds the Azure subscription, the Trusted Signing account and the identity
validation. Trusted Signing issues short-lived certificates (a few days) from
a Microsoft CA in the Trusted Root Program, and keeps the key in Microsoft's
HSMs. No private key or PFX ever exists outside Azure.

The workflow authenticates with **GitHub OIDC to a federated credential** on an
Azure app registration whose only role is *Trusted Signing Certificate Profile
Signer* on that one profile. No client secret is stored in GitHub. The
federated credential's subject is a GitHub environment (§3), so only the jobs
that run in it can mint the token.

**Every signature is timestamped** against Trusted Signing's RFC 3161 service.
Because the certificate expires within days, an untimestamped signature would
stop validating soon after the release. A failed timestamp fails the signature,
and that fails the build.

### 2. The tool is jsign, run from the Taskfile on the Linux runner

The Windows binaries are cross-compiled on Linux (0066 §4), so they are signed
there too. **jsign** signs them: it signs PE files on Linux and supports
Trusted Signing directly (`--storetype TRUSTEDSIGNING`, the account endpoint,
and `<account>/<profile>` as the alias), given an Azure access token. jsign
joins the Nix devShell. **osslsigncode** joins it too, as an independent
verifier for `release:verify` (§5).

Per 0066 §1, the split is the same one 787 §1 and `docker/login-action` use.
The workflow step `azure/login` (OIDC) is *where* it runs. Getting the access
token and running jsign is *what* it does, so that is a Taskfile target. The
target reads the endpoint, account and profile from environment variables that
the workflow passes in from repository variables. A developer who has those
and an `az login` session can run the same target.

### 3. Signing happens inside `release:binary`, before any digest is taken

Following 0066 §5, signing is part of the build. In `release:binary`, and in
`build:clipwatch:arch` for a release build:

1. **The clipboard helper** is signed after `go build` and before
   `discobox-gzip`, so the Linux CLI embeds the signed helper.
2. **The server** is signed after its link and before
   `discobox-server-manifest` takes its digest for the CLI's manifest. This is
   where the darwin `sign` call already sits.
3. **The CLI** is signed after its link and before `-client-asset` takes its
   digest for `discobox-manifest-<os>-<arch>.json`.
4. `release:windows-zip`, `release:manifest` and `release:installers` read
   files that are already signed, so they are unchanged.

The signing credential sits in **the Linux leg of the build, `binaries-linux`**
(amended: split out of the `binaries` matrix, so the darwin job never holds the
identity), in
`release.yml` and in `main-build.yml`. That leg gets `id-token: write`, runs in
a GitHub environment (`windows-signing`) whose deployment rule allows only `v*`
tags and `main`, and is the only job that runs `azure/login`. This narrows 787
§1's rule that builders hold no signing identity. The narrowing applies to this
Authenticode identity only: it can sign PE files and nothing else, and it can't
attest. The server's digest has to be taken from signed bytes before the CLI
links, so signing can't move out of the job that builds them (see Rejected).
The darwin leg and `ci.yml` never get the environment.

### 4. Every published build is signed once signing is configured, and a configured build can't skip it

The owner chose every published channel: tagged releases (`stable`, `beta`,
`edge`, a pinned tag) and every **main** build. Windows users on main run into
SAC the same way.

- **A configured published build must be signed.** A build is published when
  `release:binary` has a `PIN_TAG` and a `PUBLISH_REPO`, the same condition
  that already decides whether a manifest is linked. When the signing
  configuration is there, a file that fails to sign or timestamp fails the
  build, and so does a configuration with only some of its values.
- **A published build with no signing configuration is unsigned, with a
  warning** (amended, 2026-11-25). That covers a fork, which has no Trusted
  Signing profile and must not sign under discobox-ai/discobox's identity. It
  also covers discobox-ai/discobox itself until the owner's Azure account,
  profile and `windows-signing` environment exist. The owner chose this over
  failing every main build and release until then. *Revisit:* once the first
  signed release is out, discobox-ai/discobox's missing configuration fails the
  build again, as the `MAIN_RELEASE_TOKEN` rule in `main-build.yml` does.
- **Untagged builds are never signed.** That covers `ci.yml`'s per-push
  `release:build`, `task build`, and the dev loop. They gain no Azure
  dependency, and the 5,000 signatures a month in Trusted Signing's Basic tier
  aren't spent on builds nobody installs.

A build signs six files: the CLI, the server and the clipboard helper, for two
architectures. A busy month of main builds can pass the Basic quota. Overage is
billed per signature, and implementation picks the tier.

Releases cut before this lands stay unsigned, and nothing backfills them. A
binary can't be re-signed after its digest is stamped into installers, CLI
manifests and winget manifests.

### 5. `release:verify` refuses an unsigned Windows executable in a configured published build

For a published build, `release:verify` checks every `*-windows-*.exe` it
already reads with `discobox-winres verify`. It also checks the clipboard
helper inside `cli/internal/clipwatch/artifacts/` for each architecture. For
each file it runs `osslsigncode verify` and checks three things: an
Authenticode signature chained to a trusted root, a timestamp, and the
expected subject (the Trusted Signing profile's validated name). The subject
is a repository variable passed in with the rest of the signing configuration
(amended, 2026-11-25: the validated name isn't known until the owner's
identity validation finishes). When the build was signed, an unsigned file,
or one signed by another subject, fails the release before publish. The check reads the file, so it runs on the
Linux runner like the version-resource check does.

### 6. Nothing on the user's machine checks the signer, and Windows enforces it

- **`install.ps1` doesn't check `Get-AuthenticodeSignature`.** The digest
  stamped into the installer already pins the exact bytes, signature included.
  A signer check over the same bytes adds no trust, and the script also runs
  under PowerShell on Linux and macOS, where that cmdlet doesn't exist. What
  checks the signature is Windows: SAC, WDAC and SmartScreen.
- **The CLI pins no signer when it stages the server.** 787 §5's second trigger
  asks "which identity does the CLI pin" once Windows binaries carry a platform
  identity. The answer is none, for the same reason. The linked manifest's
  digest already authenticates the server's bytes, and the OS checks the
  signature when the file runs. A pinned subject would add a failure mode
  (a renamed profile, a re-validated identity) and no trust. 787 §5's first
  trigger is unchanged.
- **The installer's message** for an Application Control block is the small
  change triage split out: name SAC, say what to do, and remove the binary it
  left. Once releases are signed, that error on a signed binary means a
  stricter policy (an organisation's WDAC) rather than a missing signature. The
  message must not tell a user to turn SAC off when SAC isn't the cause.

### 7. What a signature is expected to do

- **SAC.** Microsoft documents a valid signature from a trusted CA as enough
  for SAC to allow an app, whatever its reputation. Trusted Signing is the path
  Microsoft names for SAC. So a signed release is expected to install and run
  with SAC on from the first release, with no reputation period. The
  implementing change confirms this on a machine with SAC on (the reporter's
  setup works) before #1422 closes.
- **SmartScreen** is out of scope. It acts on files with mark-of-the-web, so it
  doesn't see `irm … | iex` or winget, though it does see a zip downloaded in a
  browser. Signed files there build reputation under the signer's identity over
  time. Since 2024 an EV certificate no longer skips that, so no certificate
  choice would avoid it.

## Consequences

- With SAC on, Windows 11 runs the CLI, the server it stages, and the WSL
  clipboard helper, for every release and main build from the first signed one
  onward. The evaluator guide's Windows path no longer asks anyone to turn off
  an OS security feature.
- The owner keeps up an Azure subscription, a Trusted Signing account
  (around $10 a month for Basic, plus overage) and its identity validation,
  which comes up for renewal on Microsoft's schedule. If the validation lapses,
  the profile stops issuing certificates and every published build fails at
  §4's check until the validation is renewed.
- The `binaries-linux` job gains `id-token: write` and an environment. A
  compromise of that job can get files signed as Discobox for the duration of
  the job, the same blast radius it already has over the bytes it builds.
- Windows files are named as published by the validated identity in Explorer's
  Digital Signatures tab and in `Get-AuthenticodeSignature`.
- Signing adds a network call per file to the Linux leg. A Trusted Signing
  outage blocks a release. That is accepted, since an unsigned release is
  broken for SAC users.

## Open, for implementation

- **The identity validated**: an organisation or the owner as an individual.
  This depends on the owner's onboarding, and §5's expected subject follows
  from it.
- **The Basic or Premium tier**, once a month of main builds shows real
  signature counts.
- **Whether nixpkgs has a usable jsign.** If not, the flake packages its
  release jar, pinned by hash.
- **How the access token reaches jsign**: `az account get-access-token` for
  the `https://codesigning.azure.net` resource after `azure/login`, or jsign's
  own Azure CLI integration.
- **The Taskfile shape**: a `sign:windows` target taking a binary, in the
  style of `sign`, or one target that `sign` dispatches on the target OS.

## Rejected alternatives

**An OV or EV certificate in a cloud HSM** (Azure Key Vault, Google Cloud KMS,
a CA's own cloud signing). Since 2023, CA/B rules keep the key in an HSM, so
there is no cheap PFX in a secret to fall back on. This costs several times as
much, renews every year with re-validation each time, and leaves the owner
holding a key with its own revocation story. Since 2024, EV no longer grants
instant SmartScreen reputation, so it buys nothing over Trusted Signing for
SAC. The owner chose Trusted Signing.

**Defer signing, and ship only the installer message and the docs.** This
leaves Windows unusable with SAC on, which is the default on new machines. The
message and the docs ship anyway (§6), but they aren't a fix.

**osslsigncode as the signer.** It signs PE files on Linux, but it reaches a
cloud key only through a PKCS#11 provider or an OpenSSL engine, and Trusted
Signing offers neither for Linux that we could rely on. jsign supports Trusted Signing natively. osslsigncode stays as
the verifier, so the check doesn't use the tool that signed.

**Sign in a job of its own** that holds the identity, keeping 787 §1's rule
whole. The server must be signed before the CLI links its digest, and the
helper before the Linux CLI embeds it. A separate signer would have to sit
between the server build and the CLI link: build the server and the helper,
hand them to a signing job, then build every CLI in a third job. That is two
artifact round trips and a serial job on every release, to keep an identity
out of a job that already controls every byte it would sign. The environment's
deployment rule limits which refs can use it.

**Sign in packaging** (`release:publish`, or the winget zip). Rejected by
0066 §5: signing after a digest is taken breaks the server manifest linked into
the CLI, the CLI manifest, and the installers' stamps.

**Sign tagged releases only.** This is cheaper and keeps the identity out of
`main-build.yml`, but main users on Windows with SAC on can't run what they
installed. The owner chose every published build.

**Have the installer or the CLI require a signature from a pinned subject.**
Rejected in §6: the digests already pin the bytes, and Windows enforces the
signature itself.
