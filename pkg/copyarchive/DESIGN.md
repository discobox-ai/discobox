# Copy archive

Shared by the CLI and the default-user files helper. This is an overwrite-copy
format, separate from `sandboxtree`'s durable restore format (ADR 26-11-04-684).

- `Export` writes regular files and directories only. The first `files/`
  directory carries selected-root ordinary mode and atime/mtime; descendants
  follow under that namespace. Source links are dereferenced inside the pinned
  root with ancestry cycle checks; an explicit source-root link resolves first.
- `tarsums` supplies the final required `SHA256SUMS`, outside `files/`.
  `Import` verifies it and consumes the complete body before reporting success.
- Decoded member names are validated, never cleaned into acceptable names.
  Absolute, parent, drive/UNC, alternate separators, duplicate/colliding names,
  links and special members fail. Limits bound entry count, depth, names and aggregate metadata (64 MiB);
  Go's tar parser bounds extended-header allocation.
- `os.Root` pins source/destination trees. Caller-selected ancestor symlinks
  resolve before pinning; symlinks inside a destination tree remain refused. Each destination directory is opened
  separately, rejects symlinks, and is checked against its pinned identity.
  Regular files publish atomically through temporary siblings and rooted rename.
  Metadata uses pinned file descriptors. No external archiver or shell runs.
- Imports merge directories, preserve unrelated entries, and fail type conflicts.
  Completed files remain on any later failure, including integrity failure;
  the current truncated file keeps its prior content and temporary files go away.
- Without preservation, existing modes remain and new ordinary modes are masked
  by the receiver's umask. With preservation, apply ordinary modes and atime/mtime,
  finalizing directories after children. No ownership, ACLs or privileged bits.
  Unsupported preservation fails explicitly.
