# macOS vz Provider Review Rules

- **The host must never need Docker.** Any new host-side step that shells out to
  `docker`, or that requires a daemon on the Mac, breaks the reason this backend
  exists. Building something from local sources means building it on a pool's
  own daemon (`dockerworker.BuildArtifacts`), not on the host.
- **Never open a TCP listener.** Both directions are VSOCK. An IP listener on
  macOS is a machine-wide surface and a firewall prompt.
- **Do not add a `CloseWrite` that does nothing.** A caller that finds the
  method believes the peer saw EOF. If a code path genuinely needs half-close,
  fix it at the binding, not with a method that lies.
- **The root disk is shared and read-only.** Never attach it writable and never
  add `rw` to the kernel command line: every pool on the host has it open.
- **`StopVM` keeps the disks; only `DeleteVM` removes them.** Repair calls
  `StopVM`, and a pool's images, volumes, and containers all live on `data.raw`.
- **Ask the guest to shut down before stopping it.** A hard stop is a dirty
  unmount of both disks while Docker is writing to them.
- **The `/Users` share is read-only, at the same path, from one list.** The host
  enforces read-only; a sandbox writing to a developer's files is not a feature
  behind a flag. The guest must mount it at `/Users` and nowhere else — the
  origin bind the pool agent gives Docker is the raw host path, with no
  host-mount prefix applied — and the driver's virtiofs shares, the engine's
  host mounts, and the published `LocalSourceRoots` all come from `hostShares`.
  Setting any of them separately is how a pool ends up claiming it can clone a
  path its guest cannot see.
- **Adding the host mount needs the guest that has the mount point.** Docker
  refuses to bind a source the daemon does not have, and the guest root is
  read-only, so `/Users` exists only if the image created it. A server that
  declares the mount against an older guest fails every pool with "bind source
  path does not exist: /Users". In development the answer is
  `discobox admin pool build-guest` and a pool recreate; for a release it is a
  `vm/v*` tag and a re-pinned `DefaultGuestImage`, shipped before or with the
  server-side mount, never after.
- **Pool IDs become directory names.** Anything reaching `filepath.Join` with
  the state directory goes through `validatePoolID` first.
- **Signing is not optional.** A change to how `discobox-server` is built must
  keep `task sign` in the path, or macOS pools stop starting with an opaque
  framework error. It is the only binary that needs it: the CLI runs the server
  as a separate process (ADR 0099), so nothing it does creates a VM.
- **The guest image is not vz's to change alone.** Everything under `vm-image/`
  boots libkrun pools too, and its rules live in `vm-image/REVIEW.md` where the
  drill-down reaches them. A change there that only makes sense on a Mac is a
  change in the wrong place — including the clock step, which is conditioned on
  the RTC rather than on the backend, and the virtio module set, which follows
  what a driver attaches rather than what `vzvm.Start` alone does.
