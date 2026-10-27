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
- Generator Changesets must exclude installed dependency trees from the output
  snapshot as well as the input source. Adding pnpm generation otherwise made
  `node_modules` appear as a large source change on a clean checkout.
- Resolve Taskfile migration conflicts by preserving its complete shortcut
  inventory and moving new upstream build/generation behavior into Dagger.
  Verify `task --list`; a conflict-marker replacement truncated the command tail.
