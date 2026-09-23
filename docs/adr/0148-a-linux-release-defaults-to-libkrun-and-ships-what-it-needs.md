# 0148 — A Linux release defaults to libkrun, and the image it boots carries what it needs

- **Status**: Accepted
- **Date**: 2026-09-23
- **Supersedes**: [ADR 0101](0101-one-guest-image-for-every-vm-backend-and-the-kernel-is-separate.md)
  §3 (libkrun resolves the kernel as a second artifact) and the libkrun half of
  its §1 pin; [ADR 0113](0113-the-cli-stages-the-images-its-server-loads.md)'s
  deferred "libkrun's images are fetched through the store but not staged";
  [ADR 0013](0013-local-linux-pools-use-libkrun-microvms.md) §2's user-supplied
  runtime closure.
- **Relates to**: [ADR 0062](0062-macos-pools-run-vz-vms-with-an-independently-released-guest-image.md)
  §9, the in-server launcher and lazy dlopen this keeps; [ADR 0067](0067-iroh-ships-in-every-build.md),
  the one native library a release already carries.
- **Issue**: [#34](https://github.com/discobox-ai/discobox/issues/34)

## Context

On Linux the server installs one default provider on its first start, and it
picks it by `runtime.GOOS` alone: `docker`. A sandbox on a fresh Linux install is
a container on the host kernel, with no VM between it and the machine. macOS
(vz) and Windows (wslc) get a VM with nothing to install. The site describes
the VM as the boundary on every platform, and nothing a Linux user sees
contradicts it. A reader who believes a compromised agent is behind a VM when
it is not has been misled about the one property that matters most here.

libkrun is the Linux VM backend (ADR 0013) and already boots the same guest as
vz (ADR 0101). It is not the default for three reasons, and none of them is
the boundary:

- **The user brings its runtime.** The launcher dlopens `libkrun.so.1` and
  execs `passt` by name (ADR 0062 §9). A release ships neither, and the only
  builds the repository makes are Nix store paths, which have `/nix/store`
  RUNPATHs and interpreters and cannot be copied to another machine.
- **The Nix build of `libkrun.so.1` needs `libkrunfw.so.5` to load**, a 21 MB
  library whose only content is a kernel that discobox never boots: the
  provider hands libkrun its own kernel through `krun_set_kernel`. Upstream
  libkrun (1.19) does not need it. It opens libkrunfw lazily and treats it as
  optional (`src/libkrun/src/lib.rs`, `libloading::Library::new(...).ok()`).
  The `NEEDED` entry comes from how Nix packages it.
- **Its images are not staged** (ADR 0113, deferred until libkrun became the
  default), so a first libkrun pool downloads its guest and kernel behind a
  server that has already said it started.

Docker as the default is still the right answer while developing discobox. The
`task dev` loop converges freshly built images onto the host daemon, and a
contributor's machine should not need KVM to run the tests.

## Decision

### 1. A release build on Linux defaults to libkrun; a development build to Docker

A release build is one whose version is stamped: `version.Version`, which the
release's ldflags set from the tag and which nothing else sets. A server with
no stamped version is a development build.

On Linux, `defaultSandboxProviderForOS` installs `libkrun` in a release build
and `docker` in a development build. macOS and Windows are unchanged. The
choice still happens once, gated by the existing `server_state` row, so an
install that already has a Docker default keeps it. This decision changes only
what a first start installs.

Either build can be told to install the other one (§4). Docker stays a
supported provider. It is only no longer the default a release gives a user who
has not chosen it.

### 2. libkrun has no fallback; the server waits for a choice

When a first start would install libkrun and libkrun cannot run on the host,
the server does not install Docker in its place. The cases are:

- no usable `/dev/kvm`;
- an architecture the launcher does not support, which today is anything but
  amd64;
- a runtime that will not load.

The server owns this check, one function in `server/providers` that runs in
startup before the default provider is installed. Quietly degrading from a VM
to a container is exactly the defect this ADR exists to remove.

It does not exit either, which is how the Windows server treats a host without
WSL Containers. An exited server leaves its reason only in the log. A CLI that
starts it as a systemd user service cannot read its exit code, so the CLI could
only match log text. Instead the server stays bound and holds startup. Its
health endpoint answers a new status, `needs-choice`, carrying three things:

- a reason code: `kvm-unavailable`, `arch-unsupported` or `runtime-unloadable`;
- the detail, such as `open /dev/kvm: no such file or directory`;
- the alternative it will accept, `docker`.

The log says the same, together with the command that answers it, so a server
started by hand or by a service manager is not left waiting silently.

The check belongs to the first start only. Once a libkrun provider is installed
it is an ordinary user-owned record, and a later failure, such as KVM
disappearing, is that provider's failure. It is reported on its pools, not by a
server that will not serve.

### 3. The CLI asks

`endpoint.EnsureRunning` stops waiting when a server reports `needs-choice`. It
does not treat it as still starting or wait out a timeout. It returns a typed
error that carries the reason, the detail and the alternative.

The CLI's autolaunch (`ensureLocalServer`) owns the wording and the question.
The TUI starts the server through the same path, so it can ask the same
question as a dialog.

- An interactive CLI takes its progress line down, prints the reason, and asks:

  > libkrun cannot run here: /dev/kvm is not available. Sandboxes can run as
  > Docker containers on this machine's kernel instead, without a VM boundary.
  > Use Docker? [y/N]

  Yes sends the choice to the waiting server (§4), which installs Docker and
  finishes starting, and the CLI's normal wait carries on. Nothing is
  restarted.
- No, a non-interactive CLI, or `--quiet`: the CLI exits non-zero with the same
  reason and the command that makes the choice.

Every time it asks, the prompt names the VM boundary the user would be giving
up.

### 4. The default provider can be chosen

A choice reaches the server in one of two ways, and both affect only the
one-time install. After that, providers and pools change through the admin
commands, as they do today.

- **Answering a waiting server.** While it reports `needs-choice`, the server
  accepts one request next to health, `POST /setup/default-provider` with the
  provider type, and only over the local IPC endpoint. That is the trust every
  request the CLI makes to its own server already relies on. A remote listener
  answers it with the same 503 it gives every path during startup. A command
  (`discobox admin server choose-provider docker`) sends the same request for a
  user answering a server they started by hand.
- **Configuration.** A `defaultProvider` server setting, from the config file
  or the environment, is `libkrun` or `docker` on Linux. It chooses in advance:
  a release told `docker` never runs the check, and a development build told
  `libkrun` gets §2's behavior.

### 5. The libkrun image carries the libkrun runtime

libkrun boots one image, `discobox-vm-krun` (linux/amd64). It holds everything
the host needs to boot a pool, beside the disk the pool boots:

| File | Built by |
| --- | --- |
| `root.ext4` | the shared guest build (ADR 0101 §1), copied by digest |
| `vmlinux`, `kernel.config` | the libkrunfw-patched kernel build |
| `libkrun.so.1` | the same build, from the libkrun release its libkrunfw commit pairs with |
| `passt` | the same build |

The kernel build becomes the libkrun runtime build. It compiles the patched
kernel, libkrun, and passt together, because they change together: the
libkrunfw patches are what the libkrun version expects of its kernel. It builds
them on the base image's Debian, so they need no newer glibc than the guest's
own userland does. It builds libkrun from upstream, which loads without
libkrunfw, and it builds passt static. Its verification refuses a `libkrun.so.1` whose `NEEDED`
names `libkrunfw`, and any artifact with a `/nix/store` path in it.

Publishing the libkrun image compiles nothing. It is a `FROM scratch` image
that copies `root.ext4` from a pinned `discobox-vm` digest and the runtime
files from a pinned runtime-build digest. A guest release therefore costs
libkrun one packaging publish and one pin edit, not a kernel compile.

The provider resolves the one image through `guestimage`, extracts all five
files into its per-digest directory, dlopens `libkrun.so.1` from there, and
execs `passt` from there. `libkrunPath` and `passtPath` remain as overrides,
which is how `nix develop .#libkrun` keeps working. The provider pins one
digest, `DefaultVMImage` for libkrun. `DefaultKernelImage` goes away.

### 6. A release CLI stages it

The release manifest's image set names the libkrun image for linux/amd64 in
place of the guest and kernel it names today. On a Linux release,
`providers.DefaultBootImages` names it too. Either way the CLI stages it with
the server before the first start (ADR 0113 §1–§2). This is ADR 0113's
"revisit if libkrun becomes Linux's default".

## Consequences

- A fresh Linux install gets the same boundary as macOS and Windows: one VM per
  pool, with sandboxes as containers inside it.
- A Linux host without KVM gets a question at first run where it used to get a
  container without being asked. Many cloud VMs have no nested virtualization,
  and those users will see the prompt.
- linux/arm64 releases have no VM default until the launcher supports arm64.
  An arm64 first start takes §2's path, and the prompt offers Docker.
- A release now carries native code that discobox builds and is responsible
  for: libkrun, which is Rust, and passt. Security updates to either are a
  runtime rebuild, a packaging publish and a pin, on the libkrun image's line.
- There are two published VM images for amd64, the shared guest and the libkrun
  image that embeds it. ADR 0101 rejected two guest images because they would
  be "the same bytes … differing only in who pulls them". These are not the
  same bytes, because the libkrun image adds the host runtime. It does mean a
  guest change reaches libkrun only when the libkrun image is republished.
- The first libkrun pull is about the size of the guest plus 7 MB of libkrun,
  the kernel and passt. libkrunfw's 21 MB is not shipped.
- The issue's documentation gap closes with the code: the README states each
  OS's default boundary, and the Docker choice is described where it is made.

## Alternatives

**Keep Docker as the Linux default and document it.** Rejected: it leaves the
safest configuration opt-in on the one platform where a user can most easily
run without a VM, and every user who does not read the docs gets the weaker
boundary.

**Fall back to Docker when libkrun cannot run, with a warning.** Rejected: a
warning at first start is read once, and a fallback turns a missing VM into a
log line. §3 asks instead, because the user is the one giving the boundary up.

**Ship libkrun and passt beside the CLI, or embed them in the server binary as
iroh is (ADR 0067).** Rejected: libkrun then moves on the product's release line
rather than with the kernel it must match, every platform's download carries a
Linux-only library, and a libkrun security fix needs a product release. In the
libkrun image it is pinned together with the kernel it was built against, and
fetched only by a host that will boot it.

**Resolve the runtime as a third image beside the guest and the kernel.**
Rejected: three pins that must agree, where one image is one pin. A host
cannot use any one of the three without the other two.

**Ship libkrunfw too.** Rejected: upstream libkrun does not need it when it is
handed a kernel, and it would put 21 MB of a kernel that is never booted in
every libkrun download.

**Exit on failure, and have the CLI restart the server with Docker chosen.**
Rejected: the only record of why a server exited is its log, and a server
started as a systemd user service hides its exit code. The CLI would have to
match log text to know it should ask. It also restarts a server that was
already up and bound, only to answer one question.

**Check in the CLI before it launches, by asking the downloaded server
binary.** Rejected: only the server's database knows whether this is a first
start, so the check command would open it, and the check would exist twice,
once as a command and once in startup.

**Decide release against development at run time**, for example from whether
a release manifest is configured or `task dev` is running. Rejected: the
default is installed once and persisted, so it must come from what the binary
is, not from the environment it happened to start in. The stamped version is
fixed at build time, and only the release sets it.
