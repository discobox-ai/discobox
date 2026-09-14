# 0120 — wslc guest programs are streamed in over stdin, not mounted

- **Status**: Accepted
- **Date**: 2026-09-13

## Context

A wslc pool guest boots from a stock Microsoft image. Discobox needs a program
of its own inside that VM's root namespace in two places: the control-plane
relay (`discobox-cp-relay --socket`), and a stdio-to-socket bridge for every
Docker Engine connection. The image has neither, and has no toolchain, no
`socat`/`ncat`/`python3`, and a BusyBox `nc` without `-U`.

Both used to get there through a Windows folder shared into the guest with
`IWSLCSession::MountWindowsFolder`, one of two calls this library made on
wslc's private interface. The relay was cross-compiled and staged on the host;
the bridge was `bridge.c`, mounted as source and compiled on first dial by the
guest's own dockerd in an `alpine:latest` container.

On WSL 2.9.10 (Windows 11 26200) that mount fails inside wslc's own guest init.
The 9p options it passes (`rfdno`/`wfdno`/`cache=mmap`) are not ones this WSL
kernel's v9fs accepts, so `mount(2)` returns `EINVAL` and the call comes back
`E_FAIL`. No pool can start. The mount options are chosen by the service, not
by the caller.

## Decision

1. **Nothing is shared into the VM from Windows.** The driver streams the one
   guest program it needs, the embedded relay, to a guest `sh` reading its own
   stdin. The program is written to a staging name, made executable, renamed to
   `/tmp/discobox-cp-relay`, and its SHA-256 is echoed back and checked against
   the host's before anything runs it. This happens each time `EnsureVM` starts a VM:
   `/tmp` is tmpfs, so the program has the same lifetime as the VM.
2. **The relay is the only guest program.** It gains a one-shot
   `--dial <target>` mode that splices its stdio to one guest address. That
   mode replaces `bridge.c`, and every Docker connection is one `--dial`
   process.
3. **Docker stays off the mux.** Each Docker connection is its own guest
   process rather than a stream on the control-plane mux. Image loads and build
   contexts would otherwise head-of-line block the pool agent.
4. **The session makes exactly one private COM call,
   `CreateRootNamespaceProcess`.** `MountWindowsFolder` is removed, not
   repaired. `Session.StartProcess` is the package's whole guest-facing surface.

## Alternatives rejected

- **Fix or work around the 9p mount.** The failing options are issued by wslc's
  guest init for its own call, so there is nothing on the caller's side to
  change. Waiting for a WSL release that agrees with itself leaves every wslc
  pool broken until then.
- **Share the folder the supported way.** The SDK-facing interface shares a
  Windows folder into a *container* (`WSLCCompatVolume`), not into the VM's root
  namespace, where both the relay and the bridge have to run. Microsoft's
  sharing path has moved to virtiofs, but no call reaching the root namespace
  over it is one this library can make.
- **Keep `bridge.c`, delivered by streaming.** It still needed a compiler in the
  guest. That meant an `alpine:latest` pull through the guest's dockerd on the
  first dial of every VM boot, so the first Docker call needed network access.
  The relay is already a static Linux binary that dials the same
  `unix:`/`tcp:` targets.
- **Carry Docker traffic as mux streams.** This would need one guest process
  instead of one per connection, but bulk Docker transfers would share a single
  ordered stream with the agent's control plane (Decision §3).

## Consequences

- Every VM start transfers the relay (~2.5 MB) and verifies it, bounded by
  `runGuestCommand`'s one-minute timeout, since `guestConn` deadlines are no-ops.
- The install depends on the stock guest's `sh`, `cat`, `chmod`, `mv`,
  `mkdir`, `sha256sum` and `cut`. A guest missing one fails `EnsureVM` with the
  shell's own message.
- A guest that can no longer exec the relay has no Docker path while the mux,
  an already-running process, stays healthy. The driver condemns the VM on a
  `GuestExecError` with a positive guest errno, and `EnsureVM` replaces a VM
  whose relay has ended.
- There is one private vtable slot left to derive per WSL version, not two.

## Revisit when

wslc's SDK-facing interface offers a way to start a process in the VM's root
namespace. That would remove the last private call. A supported share into that
namespace would not: the relay would still have to be started there. It would
replace only the stdin install, which is not worth revisiting on its own.
