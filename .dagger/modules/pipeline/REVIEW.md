# Pipeline review rules

- Verify exported OCI directory permissions and timestamps as well as regular
  files. Source normalization alone leaves COPY destination directories with
  current timestamps in the beta engine, changing otherwise reusable layers.
- Preserve Dockerfile image configuration when recomposing bundle root filesystems.
  Labels alone do not carry environment, working directory, or command metadata.
- External GitHub reads need the same invocation timestamp as writes. Cached
  tag or release-asset reads can approve a moved tag or stamp obsolete digests.
- Exercise retry and failure branches against isolated destinations. A preview
  inventories publication operations but does not execute them.
- Inspect saved image trust files from a separate container. A withExec in the
  image can temporarily inject builder CAs and hide missing or contaminated
  exported roots. Compare certificate DER against that image's enabled distro
  certificates, rather than subjects, fixed counts, or another image's bundle.
- Dagger event adapters must check out full history (`fetch-depth: 0`). The beta
  workspace Git importer cannot reconstruct a shallow PR merge checkout whose
  parent objects are missing; VM checks failed before starting their builds.
- Verify downloaded toolchain executables against committed digests before
  executing them. A version argument to a mutable installer does not pin the
  installer, and toolchain containers later receive publication credentials.
- Native test coverage cannot be inferred from filenames alone. Shared files
  can branch on the OS or call native dependencies: Windows host-key ACL repair
  was omitted by a file-only selector. Keep the Windows workspace suite and
  Darwin's explicit behavioral contract alongside file-based selection.
- Do not pipe a producer into `grep -q` under `pipefail` for membership checks.
  Its early exit can make a producer's SIGPIPE look like a missing value; the
  Dockerfile import check reported an existing `pkg/hostscope` COPY as missing.
  Feed finite captured output through a here-string instead.
- Kernel builds must execute the product Dockerfile. If the engine cannot
  implement a Dockerfile transport such as checksummed HTTP ADD, adapt only
  that instruction using a digest-verified input; never copy package lists,
  build commands or output paths into a second pipeline recipe.
- Keep Cloud source administration out of CI's OIDC execution identity.
  Enabling automatic Cloud Checks is a maintainer setup operation; trying
  `dagger cloud checks on` on every push failed with unauthorized even while
  the same commit's engine-based checks succeeded.
- Enforce PR/release cache separation at the engine boundary, including SDK
  compilation, Dockerfile mounts, native helper inputs and publication tools.
  A cache name chosen by PR-controlled pipeline code is not isolation.
