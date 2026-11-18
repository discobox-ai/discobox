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
  it, needs `go tool task test:vz-stress` and `go tool task test:vz-teardown`
  passing on a real guest first.
- **A started machine is never left to the garbage collector.** `vzvm.Start`
  hands every machine it started to `keepUntilSettled`, which holds it until the
  framework reports Stopped or Error. A new path that drops a
  `*vz.VirtualMachine` — an early return, a replaced field, a second
  `NewVirtualMachine` — must go through it too, or a late state change panics
  the server ("misuse of an invalid Handle"). `task test:vz-teardown` is the
  reproduction on a real guest.
- **Every VSOCK connection is framed; never hand out a raw one.** A new port,
  a new consumer, or a change to `vzvm.Connect`/`Listen` keeps the framing
  (ADR 26-11-17-182): an unframed connection whose host reader stops blocks
  Virtualization.framework's one VSOCK queue, and the guest's CPU 0 with it,
  until the guest panics. The guest side frames because the host says so — a
  `vsock+yamux` URL the driver renders, or `discobox.vsock=yamux` on the
  kernel command line for the image's own services — so a new guest service
  needs one of the two, not a probe. `test:vz-stress` (its hairpin and
  stalled-reader case) is the reproduction.
- **A closed `vzvm.Listen` listener parks a goroutine for the server's
  life.** The binding's `Close` never closes its accept channel, so its
  `Accept` never returns, and the framed listener's accept loop stays in it;
  the listener's own `Accept` returns `net.ErrClosed`, which is what consumers
  see. Each VM start therefore leaves up to three (3001, 3005, 3006) parked
  until the process exits, as the unframed listeners' consumers did before.
  That is cheap while a pool's VM starts rarely. Do not start listeners per
  request; if VM starts become frequent, close the channel in the binding.
- **Do not add a `CloseWrite` that does nothing.** A caller that finds the
  method believes the peer saw EOF. If a code path genuinely needs half-close,
  expose the framed stream's own half-close from `pkg/vsock` (ADR 26-11-17-182
  defers it), not a method that lies.
- **The root disk is shared and read-only.** Never attach it writable and never
  add `rw` to the kernel command line: every pool on the host has it open.
- **Know which hard-lockup detector the guest's kernel has before relying on
  it.** Debian's arm64 kernel had only the buddy detector, which reads a vCPU
  the Mac deschedules for 1.2 × `watchdog_thresh` as locked. Ubuntu's arm64
  kernel, the guest's since ADR 26-11-16-365, builds the perf detector
  (`HARDLOCKUP_DETECTOR_PERF`) instead, which needs a PMU the guest can take
  an NMI from; on a vz guest without one, no hard-lockup detector runs and
  `nmi_watchdog=panic` does nothing. Read the guest's boot log (its
  `NMI watchdog` line) on a Mac after a guest kernel change. Whichever
  detector runs, `nmi_watchdog=panic` turns what it reports into a reset, so
  lowering `watchdog_thresh` or making soft lockups or RCU stalls fatal can
  reset pools under host load.
- **A panicked guest reboots inside the same VM.** Virtualization.framework
  does not stop a VM whose guest panics: `Running` stays true, and the guest's
  own reboot is what recovers the pool, not repair. Do not write recovery code
  or docs that wait for the VM to stop on a panic; that belief was written into
  four places before a real Mac showed otherwise.
- **A host-side cache of guest connections lives as long as the guest's boot,
  not the VM's.** A panicked guest reboots inside the same VM, and nothing
  promises the old boot's connections end with it: a request left on one can
  wait until its caller gives up. Anything that keeps VSOCK connections across
  requests must drop them, in flight included, when the guest refuses a dial,
  as `dockerworker.PoolAgentClient` does (#1335).
- **A host-to-guest dial is bounded in `vzvm.(*VM).Connect`, and takes its
  caller's context.** The framework may never complete a connect to a stalled
  guest. Three dialers once discarded their context and left dials parked past
  the VM's replacement (#1336); a new dialer passes the context it is given,
  never `context.Background()`. That context is not enough alone: an
  `http.Transport` dials with one detached from the request, so a request that
  gives up does not end its dial. Do not remove `connectTimeout` on the
  grounds that callers pass a context.
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
