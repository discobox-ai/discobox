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
- **Disk order is a contract.** Root, data, cache map to `vda`, `vdb`, `vdc`,
  and the guest's storage unit hard-codes that. Reordering or inserting a disk
  silently mounts the wrong filesystem.
- **`StopVM` keeps the disks; only `DeleteVM` removes them.** Repair calls
  `StopVM`, and a pool's images, volumes, and containers all live on `data.raw`.
- **Ask the guest to shut down before stopping it.** A hard stop is a dirty
  unmount of both disks while Docker is writing to them.
- **Pool IDs become directory names.** Anything reaching `filepath.Join` with
  the state directory goes through `validatePoolID` first.
- **Signing is not optional.** A change to how either binary is built must keep
  `task sign` in the path — for `disco` as much as for `discobox-server`, since
  `disco` runs the server in-process — or macOS pools stop starting with an
  opaque framework error.
- **Do not replace the clock step with NTP, or make it conditional.** The guest
  is hours off precisely when the Mac has slept, which is the case an NTP
  daemon refuses to correct on its own. Every 401 in both directions traces
  back here.
- **A new guest device is three edits, not one.** The image ships drivers for
  the six virtio devices `vzvm.Configure` attaches and deletes the rest, so
  attaching a device the guest has never had also means keeping its module class
  in `image/Dockerfile` and, if the root depends on it, adding it to
  `/etc/initramfs-tools/modules`. A missing module is a guest that boots to no
  device or does not boot at all.
- **Guest image changes are a separate release.** Editing `image/` does not ship
  with the server; it ships when a `vm/v*` tag is cut and `DefaultGuestImage` is
  re-pinned to the new `discobox-vm` digest.
