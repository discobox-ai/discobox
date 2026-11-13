# macOS vz Provider Review Rules

- **The host must never need Docker.** Any new host-side step that shells out to
  `docker`, or that requires a daemon on the Mac, breaks the reason this backend
  exists. Building something from local sources means building it on a pool's
  own daemon (`dockerworker.BuildArtifacts`), not on the host.
- **Never open a TCP listener.** Both directions are VSOCK. An IP listener on
  macOS is a machine-wide surface and a firewall prompt.
- **The framework owns a VSOCK connection's descriptor.** A binding may dup it
  but never close or adopt it: the framework closes it again on dealloc, into
  whatever reused the number. Moving off the `discobox-ai/vz` fork, or bumping
  it, needs `go tool task test:vz-stress` passing on a real guest first.
- **Do not add a `CloseWrite` that does nothing.** A caller that finds the
  method believes the peer saw EOF. If a code path genuinely needs half-close,
  fix it at the binding, not with a method that lies.
- **The root disk is shared and read-only.** Never attach it writable and never
  add `rw` to the kernel command line: every pool on the host has it open.
- **The guest's hard-lockup detector is timer-based, not an NMI.** Debian's
  arm64 kernel has only the buddy detector, which reads a vCPU the Mac
  deschedules for 1.2 × `watchdog_thresh` as locked. Since
  `nmi_watchdog=panic` turns that into a reset, lowering `watchdog_thresh` or
  making soft lockups or RCU stalls fatal resets pools under host load.
- **Writable disks attach cached, with fsync sync.** The framework's default
  attachment corrupts ext4 on Apple silicon; do not go back to
  `NewDiskImageStorageDeviceAttachment` for a new disk.
- **`StopVM` keeps the disks; only `DeleteVM` removes them.** Repair calls
  `StopVM`, and a pool's images, volumes, and containers all live on `data.raw`.
- **Ask the guest to shut down before stopping it.** A hard stop is a dirty
  unmount of both disks while Docker is writing to them.
- **The `/Users` share is read-only, at the same path, from one list.** The host
  enforces read-only; a sandbox writing to a developer's files is not a feature
  behind a flag. The guest mounts it at `/Users` and nowhere else, and the
  driver's virtiofs shares and the engine's host mounts both come from
  `hostShares`. No source delivery reads the share
  ([ADR 26-10-10-545](../../../docs/adr/26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md)):
  do not add a path that serves or clones a source from it.
- **Adding the host mount needs the guest that has the mount point.** Docker
  refuses to bind a source the daemon does not have, and the guest root is
  read-only, so `/Users` exists only if the image created it. A server that
  declares the mount against an older guest fails every pool with "bind source
  path does not exist: /Users". In development the answer is
  `discobox admin pool build-guest` and a pool recreate; for a release it is a
  `vm/v*` tag and a re-pinned `guestimage.DefaultVMImage`, shipped before or with the
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
