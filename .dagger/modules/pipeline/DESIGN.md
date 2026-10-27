# CI and release pipeline

The module owns orchestration and release policy. Product-owned Dockerfiles,
Go/Node generators, and installer/formula generators remain canonical inputs.
There is no Task invocation, Actions API dispatch, host build dependency, or
implicit credential lookup in the graph.

- `main.go`: complete Linux toolchain, source snapshots, module discovery from
  `go.work`, checks, native-worker boundaries, and CI aggregation.
- `release.go`: embedded helpers, cross-builds, Windows resources/archives,
  final-server-digest-before-CLI ordering, receipts, complete-release assembly.
- `images.go`: OCI builds, per-feature timestamp normalization, neutral harness
  manifests versus multi-platform product indexes, and independent VM lines.
- `cmd/normalize-oci`: clamps every exported layer entry, including directories,
  to the canonical epoch without flattening feature layers. Recomputes layer
  digests, rootfs diff IDs, config and manifest references before import.
- `publish.go`: publication gates and explicit credentials, direct Homebrew
  updates, stable promotion, manual winget submission, and local preview.
- `generate.go`: source edits, including bundled harness settings parsers/licenses,
  returned as Changesets; checks never mutate the
  developer's checkout. SDK generation is a separate `go-sdk` operation.
- `trust.go`: inspects the base image's saved public roots and runs the actual
  core provisioning script from the platform bundle. Checks the exported store
  in a separate container, then exercises runtime CA addition and HTTPS using
  the application's `runcca.MaterializeTrustStore`. CI exports `trust.json`.
- Harness Dockerfiles build from the repository root to include their shared
  capture runtime and bundled settings parsers.
- Shell files implement domain checks and package-manager interactions, never
  pipeline ordering. SDK-generated files are committed and checked for drift.

The source snapshot excludes secrets, build output, and the Dagger module.
A synthetic Git repository inside the container lets existing tracked-file
checks run without mounting the operator's Git configuration or credentials.
The real source revision is supplied separately by Workspace.git and appears
in manifests and receipts. Publishing rejects dirty source snapshots.

Native execution belongs to conventional Actions jobs or the same local Go
command on macOS/Windows (`internal/cmd/discobox-native-ci`). Dagger exports
portable helpers; Mac consumes them, builds with the Apple SDK, signs the
server before hashing it into the CLI manifest, signs the CLI, and records
signing/entitlement/smoke checks and final digests. Darwin tests/vet select packages whose Go/cgo/test file lists differ from
Linux plus an explicit native behavioral contract in the host command. Windows
keeps workspace tests, including shared-file ACL/process tests through native
dependencies. The complete Linux suite stays in Dagger. CI Windows tests and smokes
Linux's exact cross-built executables. Native plans are informational, never
verification results.

Release and optional complete-preview inputs accept only named Mac assets and
its receipt. Assembly requires matching repository, version, revision, platform,
required Mac checks and final hashes for all five platforms. No native input
can overwrite Linux/Windows artifacts. Missing native artifacts fail closed.

The nested Dagger CLI is extracted from the versioned official archive only
after its SHA-256 matches the committed architecture-specific digest. No
mutable remote installer is executed. Event adapters provide full Git history
for the workspace Git importer.

Engine selection belongs to the caller. Trusted Actions jobs select Dagger
Cloud with GitHub OIDC (`DAGGER_CLOUD_TOKEN=oidc`, job-scoped `id-token: write`)
so cache persists across ephemeral runners; fork and Dependabot PRs use local
engines with Cloud authentication disabled. Native jobs do not
receive the Cloud token. Local commands can select either backend without
changing the pipeline. Actions retains event ownership; Cloud Checks are off.

Build operations are cached by Dagger. API state reads and state-changing CLI
operations use an invocation timestamp to avoid caching external state.
Registry publishing uses Dagger's publication API. Versioned artifacts publish
before mutable latest aliases. Homebrew retries normal fast-forward pushes;
winget remains an explicit manual operation. Credentials are Secret arguments
and are only mounted for the operation that needs them.

`preview` builds all Linux and Windows binaries, manifests, and ZIPs. Its
`--images` option adds real OCI builds. `ci` runs checks and those cross-builds.
VM checks inspect guest filesystems and kernel configuration; they do not claim
to boot a hypervisor. Actions adapts portable operations and transports native artifacts; native
commands run directly on their operating systems.
