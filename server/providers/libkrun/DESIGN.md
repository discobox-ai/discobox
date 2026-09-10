# libkrun Provider Design

This package implements ADR 0013 as amended by ADR 0062 §9. It is a
`dockerworker.Driver`: the shared engine still owns the pool-agent container and
Docker behavior, while this package owns one local libkrun microVM per pool.

## The invariant

A Linux machine needs `discobox-server`, KVM, and two things the launcher
reaches at run time: `passt` on `PATH`, and `libkrun.so.1` somewhere the dynamic
loader looks. Nothing else, including for guest artifacts: the root filesystem
and the kernel are pulled by digest from a registry, and no image is built on
the host to start a pool.

Both are dlopened or exec'd by name, never linked, so "somewhere the loader
looks" is the whole of the install contract and a Nix store path does not
satisfy it by itself. `nix develop .#libkrun` sets `LD_LIBRARY_PATH` and is the
development answer; `nix build .#libkrun-runtime` builds the same closure for a
machine that is not in a dev shell, which then names the library in the
provider's `libkrunPath` or puts it on the loader's path itself.

Everything is ordered off that:

1. `guestimage` pulls `root.ext4` from the shared guest image and `vmlinux` from
   the kernel image, and caches both by digest.
2. The launcher child boots the VM and its Docker daemon comes up.
3. The engine's development image build-mode builds the pool, sandbox-base, and
   harness images on that daemon's BuildKit, from the local checkout.
4. The engine starts the pool-agent container from the image it just built.

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

**The VM dies with the server.** Two mechanisms, both needed:
`PR_SET_PDEATHSIG` is armed by the kernel and survives anything the server does
afterwards, including being `SIGKILL`ed, but cannot cover the window between
fork and `prctl`; a pipe whose write end the driver holds closes when the
*process* exits, whether or not the child ever armed a signal. Between them no
VM outlives its server. There is no runtime lock, no recorded process identity,
and no re-adoption — the lifetime rule `vz` and `wslc` get for free by keeping
the VM in the server process.

`StopVM` requests an orderly poweroff and preserves `data.raw` and `cache.raw`
for repair. `DeleteVM` is reserved for an authorized pool deletion and removes
them.

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

Two images, resolved by `server/providers/guestimage`:

- **`root.ext4`**, from the shared `discobox-vm` image
  (`guestimage.DefaultVMImage`, `linux/amd64`). Read-only, shared by every pool
  on the host, and the same artifact set `vz` boots on arm64.
- **`vmlinux`**, from `discobox-vm-kernel` (`DefaultKernelImage`). libkrunfw's
  patched kernel, which is the one thing this backend cannot take from a guest
  image every backend shares: no distribution kernel boots under libkrun. The
  guest image's own kernel and initrd are never asked for.

Disks are attached in a fixed order the guest depends on — root, data, cache
become `/dev/vda`, `/dev/vdb`, `/dev/vdc` — and all three are raw. Nothing here
reads QCOW2 any more.

Sizing comes from the host: every vCPU and half the memory
(`krunvm.DefaultHostResources`), a 100 GiB data disk, a 32 GiB cache disk. None
of it is a reservation — the disks are sparse and the guest has a balloon. Disk
sizes are ceilings a pool can be given more of: `ensureSparseImage` grows an
existing image when the configured size is raised and never shrinks one, the
guest formats each disk on first boot, and runs `resize2fs` on every mount so
the filesystem follows. No `mkfs.ext4` runs on the host.

Both pins are ahead of their publishes. `discobox-vm` has only ever been cut for
arm64, so resolving it on amd64 fails saying exactly that — `guestimage` checks
the image's declared architecture, because a single-architecture manifest is
returned whatever platform was asked for — and `discobox-vm-kernel` has not been
cut at all. Until a `vm/v*` release carries both architectures and a
`vm-kernel/v*` release exists, a machine runs `task build:vm-guest` and
`task build:vm-kernel`, whose output the resolver prefers over anything
published. That is the ordinary ordering for a separately released guest: the
artifact ships first, then the pin moves.

`GuestImageBuildSpec` builds the guest image on the pool's own Docker and
exports it back, the same loop macOS needs (ADR 0062 §7). It is not
macOS-specific: it answers on a pool whose agent never started, which is the
pool a broken guest image produces. The kernel is not buildable this way — it
has its own image and its own clock.

## Storage layout

| Path | Holds |
| --- | --- |
| `<stateDir>/<poolID>/{data,cache}.raw` | the pool's durable and disposable disks |
| `<default stateDir>/.images/{guest,kernel}/` | pulled images, one directory per digest, and `local/` for a local build |
| `<runtimeDir>/<poolID>/` | sockets, the manifest, `console.log`, `launcher.log`, `passt.log` |

`stateDir` defaults under `XDG_DATA_HOME` and keeps using a pre-rename
`local-vm` directory when one exists, so disks created under the old provider
name are still found.

The image cache follows neither that fallback nor a configured `stateDir`: it is
always under the default directory, because `task build:vm-guest` has to write
where the resolver reads and a task cannot know one provider instance's
configuration. Configure `imageCacheDir` to move it. The leading dot is what
keeps `.images` from colliding with a pool in the default layout — a pool ID
must start with a letter or a digit, so no pool can take that name — and where
`stateDir` is configured elsewhere the two are not in the same directory at all.

`runtimeDir` is expected to be tmpfs. Nothing under it outlives a reboot and
nothing under it needs to; the console log survives the VM, which is the point,
because the boot worth reading is the one that did not finish.
