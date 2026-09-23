# libkrun Provider Design

This package implements ADR 0013 as amended by ADR 0062 §9 and ADR 0148. It is a
`dockerworker.Driver`: the shared engine still owns the pool-agent container and
Docker behavior, while this package owns one local libkrun microVM per pool.

## The invariant

A linux/amd64 machine needs `discobox-server` and KVM. Nothing else: the root
filesystem, the kernel, libkrun, and passt all arrive in one image, fetched by
digest through the server's image store — staged by a release CLI before the
server starts, otherwise from a registry on first use (ADR 0148 §5–§6). Nothing
is built or installed on the host to start a pool.

libkrun is dlopened and passt exec'd by path, from the directory the image was
extracted into, so neither has an install location. `libkrunPath` and
`passtPath` override them — `nix develop .#libkrun` supplies Nix-built ones —
and are otherwise unset.

This is what makes libkrun Linux's default in a release build
(`service.defaultProviderType`). A first start that would install it runs
`CheckHost` — the platform, `/dev/kvm`, and the image's libkrun loaded in a
launcher child — and a host that fails holds the start for a choice rather than
installing Docker in its place (ADR 0148 §2; see
[Startup and Readiness](../../DESIGN.md#startup-and-readiness)).

Everything is ordered off that:

1. `guestimage` resolves the libkrun image through the server's image store
   (ADR 0113 §5) and extracts its four artifacts, one directory per digest. The
   pool reports this as `sandbox.PoolPhaseFetchingVMImage`, with byte counts
   while anything is downloaded.
2. The launcher child boots the VM and its Docker daemon comes up.
3. With development image sync on, the engine converges the watcher's pool,
   sandbox-base, and harness images onto that daemon — copied from the host
   daemon (copy-mode, the Linux default) or built on the guest daemon's
   BuildKit from the local checkout (build-mode). See
   [Development Image Convergence](../DESIGN.md#development-image-convergence).
4. The engine starts the pool-agent container.

## Process boundary

`krun_start_enter` consumes the process that calls it, so a VM needs a process
of its own. That process is **this binary re-executed** — a hidden
`__pool-vm-launcher` subcommand, forked from `os.Executable()` — not a second
artifact to build, ship, and find on `PATH` (ADR 0062 §9). `discobox-server` is
the only binary that recognises it, because it is the only one that hosts a VM:
the CLI runs the server as a separate program it downloads (ADR 0099).
`RunVMLauncherIfInvoked` runs before the server starts, so a launcher never
opens a database or binds a listener.

libkrun is **dlopened in that child, never in the server**. The server binary
carries no link-time native dependency, a machine that never enables this
provider needs nothing installed, and a fault inside libkrun takes down one
pool's VM rather than the control plane. `internal/krunvm` is the whole of that
surface — the manifest, the KVM check, passt, and the purego bindings — isolated
exactly as `vz/internal/vzvm` is, so the driver, its configuration, and its
tests compile and run on every platform.

The two halves meet at a manifest file, `config.json` in the pool's runtime
directory, versioned by `krunvm.ConfigVersion` and validated on both sides.
Before starting a launcher the driver deletes the sockets the VM owns
(`Config.OwnedSockets`); their reappearance is the readiness signal, and a
launcher that exits first fails `EnsureVM` with the tail of `launcher.log`.

`krunvm.Supported` is a platform gate only: the provider is registered
everywhere, `NewDriver` refuses off linux/amd64 with `krunvm.ErrUnsupported`,
and `/dev/kvm` is checked by the launcher immediately before boot, never at
configuration time.

**The VM dies with the server.** Two mechanisms, both needed:
`PR_SET_PDEATHSIG` is armed by the kernel and survives anything the server does
afterwards, including being `SIGKILL`ed, but cannot cover the window between
fork and `prctl`; a pipe whose write end the driver holds closes when the
*process* exits, whether or not the child ever armed a signal. Between them no
VM outlives its server. There is no runtime lock, no recorded process identity,
and no re-adoption — the lifetime rule `vz` and `wslc` get for free by keeping
the VM in the server process.

`StopVM` requests an orderly poweroff over the lifecycle socket, escalating to
`SIGTERM` and then `SIGKILL`, and preserves `data.raw` and `cache.raw` for
repair. `InspectVM` reports a pool with no running VM but a data disk as
stopped, so the engine replaces the VM in place and keeps its disks. `DeleteVM`
is reserved for an authorized pool deletion and removes the disks and the
runtime directory.

## Transport boundary

Every byte is VSOCK, and libkrun terminates each port at a Unix socket:

| Port | Direction | Terminates at |
| --- | --- | --- |
| 3001 | guest → host | the server's own listening socket |
| 3002 | host → guest | `pool-agent.sock`, private to the pool |
| 3003 | host → guest | `lifecycle.sock`, orderly shutdown |
| 3004 | host → guest | `docker.sock`, the guest's Docker |

The numbering matches the vz guest, because it is the same guest. The
host-listening sockets must live under the pool's runtime directory, which is
what keeps one pool's Docker out of reach of anything that did not start it; the
manifest refuses a configuration that puts one elsewhere.

The implicit VSOCK device libkrun would add is TSI, which terminates guest
sockets on the host's network stack. It is disabled: this guest reaches the host
only through the explicit mappings above, and the outside world only through
passt.

passt is outbound-only, unprivileged user-mode networking — no TAP, TUN, veth,
or bridge appears on the host. Every pool uses the same fixed private addressing
because the guests share nothing: one passt process serves one VM over one Unix
socket. The guest takes that addressing from passt's own DHCP server rather than
configuring it by hand, which is what lets it boot the same image a
Virtualization.framework guest does (ADR 0101 §2). Do not enable TSI or add a
host TAP/TUN/veth device.

## Guest artifact boundary

One image, `discobox-vm-krun` (`DefaultImage`, `linux/amd64`), resolved by
`server/providers/guestimage` (ADR 0148 §5). It packages two builds without
compiling either:

- **`root.ext4`**, copied from the shared `discobox-vm` guest — the same root
  filesystem `vz` boots on arm64. Read-only, shared by every pool on the host.
  The guest's own kernel and initrd are not in the image.
- **`vmlinux`, `libkrun.so.1`, `passt`**, from the libkrun runtime build
  (`vm-image/libkrun`): libkrunfw's patched kernel, which no distribution kernel
  can stand in for, the upstream libkrun that boots it — which needs no
  libkrunfw, being handed its kernel — and a static passt.

Disks are attached in a fixed order the guest depends on — root, data, cache
become `/dev/vda`, `/dev/vdb`, `/dev/vdc` — and all three are raw.

Sizing comes from the host: every vCPU and half the memory
(`krunvm.DefaultHostResources`), a 100 GiB data disk, a 50 GiB cache disk. None
of it is a reservation — the disks are sparse and the guest has a balloon. Disk
sizes are ceilings a pool can be given more of: `ensureSparseImage` grows an
existing image when the configured size is raised and never shrinks one, the
guest formats each disk on first boot, and runs `resize2fs` on every mount so
the filesystem follows. No `mkfs.ext4` runs on the host.

The image is one pin because a host can use none of the four without the
other three, and `guestimage` checks its declared architecture, because a
single-architecture manifest is returned whatever platform was asked for. A
release server boots the image its release manifest names and never a local
build. Otherwise a complete local build from `task build:vm-krun` lands in the
`local/` directory the resolver prefers over the published image, and a
resolve failure names that task. `vmImageDir` instead asserts a directory and
fails when it is incomplete. A saved configuration still carrying the
superseded `guestImage*`/`kernelImage*` keys loads with a warning naming them —
it is the user's record, and a provider that stopped loading would take its
pools with it — and a write that sets one is refused. The caches they filled,
`.images/{guest,kernel}`, are no longer read.

`GuestImageBuildSpec` builds the guest (`vm-image/Dockerfile`, `linux/amd64`)
on the pool's own Docker, completes it with the kernel, libkrun, and passt of
the image this driver resolves today, and exports the whole into the
resolver's `local/` directory — the same loop macOS needs (ADR 0062 §7). It is
not macOS-specific: it answers on a pool whose agent never started, which is
the pool a broken guest image produces. The kernel and runtime are not
buildable this way; they have their own build and their own clock.

## Storage layout

| Path | Holds |
| --- | --- |
| `<stateDir>/<poolID>/{data,cache}.raw` | the pool's durable and disposable disks |
| `<default stateDir>/.images/vm/` | artifacts extracted from each fetched digest, and `local/` for a local build |
| `<runtimeDir>/<poolID>/` | `passt.sock` and the host-listening VSOCK sockets, the manifest `config.json`, `console.log`, `launcher.log`, `passt.log` |

`stateDir` defaults under `XDG_DATA_HOME` and `runtimeDir` under
`XDG_RUNTIME_DIR`; each keeps using a pre-rename `local-vm` directory when one
exists, so disks created under the old provider name are still found.

The image cache follows neither that fallback nor a configured `stateDir`: it is
always under the default directory, because `task build:vm-krun` has to write
where the resolver reads and a task cannot know one provider instance's
configuration. This provider's `imageCacheDir` moves that extraction cache —
the compressed images themselves live in the server's image store, the server
configuration's own `imageCacheDir` — and the `local/` build directory moves
separately, with `vmImageLocalDir`. The leading dot is what
keeps `.images` from colliding with a pool in the default layout — a pool ID
must start with a letter or a digit, so no pool can take that name — and where
`stateDir` is configured elsewhere the two are not in the same directory at all.

`runtimeDir` is expected to be tmpfs. Nothing under it outlives a reboot and
nothing under it needs to; the console log survives the VM, which is the point,
because the boot worth reading is the one that did not finish. `PoolLogs`
tails it.
