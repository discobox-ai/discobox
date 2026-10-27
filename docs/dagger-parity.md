# Dagger migration parity audit

Current native follow-up (2026-10-27): conventional `native-macos` and
`native-windows` Actions jobs now invoke locally runnable repository commands.
The release transfers signed Mac output and source-bound receipts into Dagger.
The historical stub references below describe the original audit snapshot;
implementation is not evidence that those new native jobs have executed.

Audit date: 2026-10-26. Baseline: Git commit
`d61a7f405389fcc0dda68c5689397aec3a085d9e`, using its original Taskfile and
all five workflows. The working tree contains the Dagger migration.

This audit separates source inspection, executed comparisons, isolated
publication tests, and deliberately unimplemented native work. A passing local
preview does not establish complete release parity.

## Release artifact comparison

The original Taskfile was exported with `git show HEAD:Taskfile.yml` into an
isolated copy of the current application source under `build/parity/source`.
The old `release:build`, `release:manifest`, and `release:installers` targets ran
inside the same Go 1.27.1 Dagger toolchain as the new graph, with
`RELEASE_TAG=v0.0.0-preview`, repository `discobox-ai/discobox`, and registry
`ghcr.io/discobox-ai`. Both paths used `GOFLAGS=-buildvcs=false -p=4`, so this isolates recipe
parity rather than reproducing every property of the old Nix runner.
The production Dagger graph never invokes Task; this
was an audit harness for the old implementation.

Compared with a fresh Dagger `preview --version=v0.0.0-preview`:

| Output | Executed comparison |
| --- | --- |
| Linux amd64/arm64 CLI and server, four files | Byte-for-byte identical |
| Windows amd64/arm64 CLI and server, four files | Byte-for-byte identical |
| `install.sh`, `install.ps1` | Byte-for-byte identical |
| Two Windows ZIPs | Identical root `discobox.exe` payloads; ZIP timestamps differ |
| Four platform manifests and `release.json` | Equal after removing only top-level `revision` |
| Revision provenance | Old isolated Git snapshot has its own commit; Dagger records the real checkout commit |
| SHA256SUMS, receipts, native reports | New integrity/status artifacts; no baseline counterpart |
| Darwin artifacts | Absent by design; native worker is stubbed |

Binary identity also covers the embedded helpers, version/resource/linker
settings, server image pins, autolaunch setting, and embedded server manifest.
It does not prove behavior on Windows or under a hypervisor.

Local evidence: `build/parity/old-artifacts`, `build/parity/new-artifacts`,
`/tmp/discobox-parity-baseline.log`, `/tmp/discobox-parity-new.log`.
These build outputs and logs are ignored, local evidence, not committed fixtures.

## Old operation to new operation

“Executed” refers to local execution of the new operation; exact old/new
output comparison is identified separately above and in the OCI section.

| Original entry point / behavior | Dagger owner | Evidence or intentional difference |
| --- | --- | --- |
| CI `ci:check` / `check`, lint every module | `static` | Executed; same pinned linter and module configs; modules discovered from go.work |
| `check:deadcode` | `static`, original `scripts/deadcode.sh` | Executed, retains platform scan |
| `check:windows` | `static` | Executed Windows amd64 vet/typecheck, CGO disabled |
| `check:dockerfiles`, `check:dockerfile-deps` | `dockerfiles`, `dockerfiles.sh` | Executed source-path and imported-package coverage checks |
| `check:shell` | `shell` / `static` | Executed original shellcheck script |
| `ci:test` / `test:all` | `tests` | Executed all seven workspace modules, same package set |
| `test` | `tests` | Intentional broader shortcut: now all workspace modules, rather than root alone |
| `fmt:verify`, `tidy:verify`, `generate:verify`, `verify:unchanged` | `verify` | Executed; container Git detects changes and new generated files |
| `check:mermaid` | `mermaid` / `verify` | Executed frozen pnpm install and original validator |
| `fmt`, `tidy`, `generate`, `check:fix` | Changeset functions | Source edits can be applied locally; no Git commit is created |
| Native `check:darwin`, `ci:darwin` | `macos` | Explicit stub; original Darwin lint/vet, cgo build, ad hoc codesign, smoke checks remain required |
| macOS full test suite | `macos` contract | Additional intended coverage; original macOS CI did not run full tests; currently stubbed |
| Native Windows CI `ci:test` | `windows` | Explicit stub, no native Windows execution claimed |
| `release:build:linux`, `release:binary` | `release-build` | Exact executable comparison above, all four Linux/Windows targets |
| `build:cp-relay`, `build:clipwatch` dependencies | `withHelpers` | Both architectures built, compressed and embedded; retained minimum-size checks |
| `release:build:darwin`, `sign` | Native worker boundary | Stub; no fake macOS executable or signature |
| `release:windows-zip` | `release-build` | Exact archived payload comparison above |
| `release:verify` | Build smoke checks and `release-assemble` | ELF interpreters, Windows resources, helper sizes, native Linux version/manifest checks; assembly now requires every platform receipt |
| `release:manifest`, `release:installers` | `release-assemble` / preview assembly | Original generators; compared outputs above |
| `release:publish` | `release-publish` | Same verify-tag/prerelease/generated-notes/clobber policy; isolated retry/failure tests |
| `release:dot`, `release:require-dot` | Version policy | Dot/explicit-prerelease validation tests; invalid versions fail before writes |
| `release:images`, `release:image`, `release:harness-bundle` | `image`, `images-build`, `images-publish` | Same 11 Dockerfile-owned products; OCI comparisons below |
| OCI `latest` for dot releases only | `release` / `latest` | Source policy inspected; real disposable registry alias/index test; no production alias moved |
| `brew:formula` | `brew-formula` | Original generator, actual downloaded release assets at runtime |
| `brew:publish` | `brew-publish` | Direct one-formula override and explicit prerelease opt-in retained; real remote push not exercised |
| `brew:refresh` workflow dispatch | `brew-refresh` | Intentionally replaced by direct Git update; local bare-Git retry/no-op/failure tests |
| Stable release event / manual promotion | `promote` | Dot/draft/tag/idempotency policy tests; calls current pipeline, not historical tag's Taskfile |
| `winget:manifests` | `winget-manifests` | Original archive-reading generator retained |
| `winget:publish` | `winget-publish` | Isolated new/existing/already-published/permission/error tests; still manual |
| Guest `vm:build`, `vm:verify` | `vm-build`, `vm-check`, kind guest | Real amd64 and arm64 image/file checks; no VM boot |
| `vm:build-kernel`, `vm:verify-kernel` | `vm-build`, `vm-check`, kind kernel | Real pinned-source amd64 kernel compile and config/ELF checks; no VM boot |
| `vm:publish`, `vm:publish-kernel`, `vm:push` | `vm-publish` | Independent version lines preserved; publication implementation inspected, no production push |
| Weekly guest / monthly kernel schedule | VM Actions adapters | Same schedules, build/verify only; .dagger changes additionally trigger PR checks |
| Depot native multiarch / buildx fallback | Dagger engine | Intentional execution change; local foreign runtime stages use binfmt; guest filesystem assembly uses native host tools |
| Actions upload/download between build jobs | Dagger Directory objects / optional export | No Actions artifact dependency in pipeline; Actions still uploads browseable results |
| Nix setup composite action | Official Dagger install action | Actions YAML statically validated; no Actions event dispatched during audit |
| Local dev / Docker / Bats / hypervisor / judge targets | Existing local Task targets | Retained host development workflows; not previously all in CI and not newly claimed as portable CI coverage |

Thin Task aliases intentionally adopt Dagger arguments, such as `--version`,
instead of preserving the original Task variable/positional argument interface.
`release:images` now builds locally; `images:publish` explicitly publishes.
Formatting/tidy verification aliases run the complete consistency check.

## OCI comparison and fixes

The old buildx recipe was exercised locally against the same Dockerfiles,
contexts, targets, compact metadata, platform selection, and canonical epoch.
It used an OCI exporter in place of its production push exporter, with timestamp
rewriting for bundles. Provenance/SBOM were disabled in the comparison harness
to compare runnable images; the original recipe disables them explicitly only
for neutral harnesses. This
compares image contents/configuration; it does not reproduce Depot or GHCR.

All 11 products were exported with the old path. New AMD64 and ARM64 builds
were exercised. The initial comparison found:

- COPY destination directories retained current timestamps even when their
  source files were normalized. `/features` was also created with mode 0644.
- Recomposition copied labels but discarded Dockerfile environment and working
  directory settings. Metadata JSON key ordering also changed label bytes.
- Initial OCI validation checked file timestamps but excluded directories.

The pipeline now retains the Dockerfile configuration, uses mode 0755 for the
feature parent, preserves metadata JSON ordering, and normalizes exported
layer archives before import. The normalizer updates compressed layer digests,
rootfs diff IDs, config, and manifest references. It preserves per-feature
layer boundaries; it does not flatten the bundle. The validator checks directory
timestamps too. A deterministic/idempotent normalizer test covers executable
contents, modes, directories, config preservation, and multiple layers.

The shell harness intentionally gains an empty `/features` directory so that
Dagger can export its otherwise empty scratch filesystem. Layer counts and
compressed digests need not match buildx: the graph recomposes feature layers.
A second independent harness export retained all three compressed layer
digests exactly (`build/parity/reproducibility.json`). The engine regenerates
image config/history timestamps on import/export, so the full manifest digest
still changes; full-image reproducibility is not claimed.

Final paths, contents, owners, modes, timestamps, architecture, and runtime
configuration are the comparison criteria. Base-image package installation
also has time-dependent files; their differences must be assessed separately
from bundle equivalence.

Executed AMD64 result: all nine nonempty bundles have identical final paths,
file bytes, symlink targets, modes, owners, timestamps, and runtime config.
The tenth bundle (shell) differs only by the empty `/features` directory.
Layer counts differ as expected (for example platform: buildx 24, Dagger 20),
so this establishes filesystem/config equivalence rather than layer-digest
identity across the two builders.

The sandbox base comparison found an additional beta frontend difference:
Dockerfile ARG values appeared in runtime environment variables. The graph
now removes build-only arguments, retaining variables explicitly assigned by
ENV. Dagger also represents the default working directory as `/`; buildx
omits it, with the same effective directory.

The base is **not byte-equivalent**. Inspection of the final local archives
corrected the earlier audit description: the old buildx archive has no saved
`/etc/ssl/certs/ca-certificates.crt` and contains two zero-byte sandbox-CA
anchor placeholders; the final Dagger archive has a bundle of 148 certificates
and no sandbox-CA anchor files. Neither the zero-byte placeholders nor their
filenames establish that a private certificate was actually shipped.

The source is unchanged: both pipelines build the sandbox base Dockerfile,
which installs the distro's `ca-certificates` package. The platform bundle
ships core's `prebuild-ca-store.sh` **script**, not a finished certificate store.
Core provisioning runs that script using the distro certificates already in
the image and saves the resulting public store under `/opt/discobox/ca-certificates`.
Runtime boot adds the running sandbox's CA through `runcca.MaterializeTrustStore`.
The prior claim that the platform bundle's finished store was compared was
incorrect; the matching bundle contains its provisioning script.

The sandbox's trust bind mounts and Dagger's temporary CA installation can
change what is visible during a build. Dagger documents that its injected CAs
are removed after execution to keep them out of published images:
[custom CA behavior](https://docs.dagger.io/reference/configuration/custom-ca/).
The local archive discrepancy is therefore not evidence that Dagger needs a
new trust source or that the old export is the correct certificate baseline.

`trust-check` now uses the actual base image and the existing script from the
actual platform image. It checks saved roots from a separate container, so
injected execution-time trust cannot hide export problems. Certificate DER
must match exactly the distro certificates enabled by the image's own
`/etc/ca-certificates.conf`; no host or toolchain CA bundle is substituted.
It also checks the provisioned store after export, then runs the production
runtime materializer and exercises real HTTPS with a disposable CA, including
unknown-CA rejection and repeated-add idempotency.

Other base differences include generated logs, ldconfig cache, and file
metadata. Their presence does not imply different runtime trust behavior.

Local evidence: `build/parity/old-oci`, `build/parity/new-oci`,
`build/parity/oci-comparison.json`, `build/parity/arm64-oci-normalized`.

The external [Homebrew tap workflow](https://github.com/discobox-ai/homebrew-tap/blob/dce0c4c71d60386eef5f620a459ef883f92d4ffa/.github/workflows/update-formula.yml)
was fetched read-only into `build/parity/baseline/tap-update-formula.yml`;
`tap-head.txt` records its branch revision. It exposed a selection mismatch:
the replacement sorted by publication time, while the old workflow uses
`gh release list` ordering, with stable limit 1 and dev limit 30. The new
script now uses the old commands and filters exactly, including its limits.

## Publication validation and limits

The pipeline test suite contains:

- Actual `release-publish.sh` execution with a recording gh fake: upload failure
  propagates; retry creates only one prerelease; every upload uses clobber.
- Actual `brew-refresh.sh` execution against a disposable bare Git repository:
  dev selection excludes explicit RCs, stable excludes prereleases, rejected
  push retries regenerate/reread state, no-op creates no commit, and three
  rejected pushes return failure without changing the remote.
- Actual `winget-publish.sh` execution with a recording gh fake: new PR, update
  existing PR, already-published no-op, insufficient scope, failed blob write.
- Promotion state tests: draft, wrong tag, explicit RC, malformed state,
  prerelease promotion, and already-stable no-op.
- The real gh API client against a local TLS mock using a dummy token: two tag
  lookups observe a changed revision. Tag/formula/winget asset reads now carry
  an invocation timestamp in Dagger so those reads are not reused across calls.
- Dagger's real publication API against a disposable registry: two-platform
  index, matching version/latest digests, and neutral single-manifest shape.

Run the isolated tests with:

```sh
dagger api with-session sh -c 'cd .dagger/modules/pipeline && GOWORK=off DISCOBOX_PIPELINE_INTEGRATION=1 go test -v ./...'
```

The ordinary module check runs the shell/API/policy/normalizer tests; the
registry integration test requires the explicit environment flag above.
The shell tests fake gh behavior, so they establish orchestration contracts,
not all live GitHub API behavior. The registry test uses small real images,
not a complete product release publication. No real GitHub release, GHCR tag,
Homebrew tap, or winget pull request was written.

Remaining gaps before treating this as a production release replacement:

1. Implement and run the macOS worker, including final signed artifacts, and
   native Windows tests. Complete release assembly intentionally remains blocked.
2. Exercise the complete release/promotion graph against dedicated GitHub,
   registry, and tap test destinations, including real permissions, tag movement,
   release upload recovery, and cross-service partial failure. Component mocks
   and the registry protocol test cannot prove that whole transaction.
3. Boot the guest/kernel outputs on their actual hypervisors if runtime image
   parity is required. Both old and new CI file inspections stop short of that.
4. Run the event adapters on GitHub and update branch protection to `pipeline`.
Provenance/SBOM parity is outside the migration acceptance criteria; no explicit
existing requirement was established, and the user excluded it.

A fake release is available locally through `preview`; it proves the portable
build/assembly portions and explicitly reports native omissions. It is not a
successful rehearsal of every publication step or a complete signed release.

## Final validation

- Original Taskfile `ci:check`, `ci:test`, and `verify`: all passed in the
  controlled same-source snapshot and toolchain. Evidence:
  `/tmp/discobox-parity-old-checks.log`.
- `dagger check`: all five checks passed (workspace tests, static checks,
  repository consistency, pipeline module tests, SDK freshness).
- `dagger api call ci export --path=build/parity/ci`: passed, including the
  current helper validation and all Linux/Windows CI cross-builds.
- Pipeline tests with `DISCOBOX_PIPELINE_INTEGRATION=1`: passed, including
  disposable registry publication and the real gh client against local TLS.
- Corrected `images-build --platform=linux/arm64`: passed all 11 image exports
  and blob/bundle validation; AMD64 exports were also validated.
- `go tool task check-hooks`: all hooks successful.

Evidence logs are `/tmp/discobox-parity-final-check.log`,
`/tmp/discobox-parity-ci.log`, `/tmp/discobox-parity-tests.log`,
`/tmp/discobox-parity-arm64-normalized.log`, and
`/tmp/discobox-parity-hooks.log`.

## Trust follow-up validation

The trust source was already correct; no replacement CA producer or host-copy
workaround was introduced. The added `trust-check` consumes the same base and
platform Dockerfile outputs and core provisioning script as the release.

`go tool task ci:trust` and the full `ci` graph passed. Both exported reports
record 148 enabled distro certificates, exact certificate-DER equality, no
extra builder roots, rejection of a deliberately contaminated public bundle,
successful HTTPS after the production runtime materializer adds a disposable
CA, rejection of a different CA, idempotent re-addition, and an unchanged
public bundle. HTTPS uses local disposable TLS servers with actual certificate
verification; this is not a public-internet connectivity or full sandbox-boot test.

Current evidence: `build/ci/trust.json`,
`build/dagger-trust/ci/trust.json`, `/tmp/discobox-trust-task.log`, and
`/tmp/discobox-trust-ci.log`. `dagger check` also passed all five checks.
