# 0096 — A source keeps its host path inside the sandbox only where a sandbox may hold it

- **Status**: Proposed
- **Date**: 2026-09-09

## Context

A local source is placed inside the sandbox at the absolute path it has on the
host (`localRunDestination`, `cli/internal/sandboxcreate/source.go`). That is a
deliberate property, stated in `discobox run --help` and in `cli/DESIGN.md`: a
path means the same thing on both sides of the sandbox boundary, so `../foo`,
a `.env` naming a directory, and a script with a hardcoded path all keep
working.

The path is not merely a label. `sandboxSources` turns it into a **mount
target**: the pool agent materializes each source into `/.discobox/sources/<slug>`,
and the sandbox-agent bind-mounts that onto the target from inside the box, as
the sandbox user (ADR 0007, `doc.Runtime.Sources`). So the host's directory
layout chooses mount points on a Linux machine the host does not own.

That machine is a systemd machine, and systemd owns some of those directories:

- The sandbox image ships **systemd 257 with `tmp.mount` enabled** — it is
  symlinked into `local-fs.target.wants/` — so a fresh tmpfs is mounted over
  `/tmp` during boot.
- `/usr/lib/tmpfiles.d/tmp.conf` carries `q /tmp 1777 root root 10d`, so
  systemd-tmpfiles ages `/tmp` out even where the tmpfs is not the issue.

A source targeted at `/tmp/...` is therefore a mount onto a directory systemd
replaces: shadowed if the bind is made before `tmp.mount`, thrown away at the
next boot if made on top of it. Observed on a discobox created from
`/tmp/vp/repo`: the box came up `running`, `/.discobox/origins/primary` was
present (it is a mount of its own, outside `/tmp`), `/tmp/vp/repo` did not
exist at all, and because the container's configured working directory pointed
at it, **every exec failed** with

```
OCI runtime exec failed: … chdir to cwd ("/tmp/vp/repo") … no such file or directory
```

A discobox with no source and no working terminal, reporting itself healthy.

This is not a new class of bug in this repository — it is the one already
handled one directory over. `sandbox-agent/boot/paths.go`, `boot/wire.go` and
`server/configuredir.go` all carry the same note about `/run`: systemd mounts
its own tmpfs over it during startup, so anything written before that is
shadowed, and the agent is ordered after it deliberately. `/tmp` is that hazard
without the handling, and `/tmp` is not alone: `/var`, `/run`, `/etc` and `/usr`
belong to the image or to systemd in the same way.

The mirror rule was written for the paths people actually keep code in — home
directories, mounted drives — where nothing inside the sandbox has an opinion
about the path. It was never a claim that *every* host path is safe to occupy
inside a sandbox; that part was assumed.

## Decision

### 1. A host path is mirrored only from a root a sandbox may hold

Six roots, and their children:

```
/home  /Users  /mnt  /workspace  /Volumes  /media
```

These are where user data lives on the platforms Discobox runs on: Linux and
WSL homes, macOS homes, mounted drives (`/mnt` is also where a Windows path
already lands — see §3), removable and external media, and the sandbox's own
workspace. Nothing in the sandbox image manages any of them.

It is a whitelist, not a list of forbidden system directories, because the two
fail in opposite directions. A forbidden-list that misses a root — `/snap`,
`/nix`, `/usr/local`, whatever the next image adds — produces exactly the
failure above: a box that comes up healthy with no source in it, discovered
much later. An allow-list that misses a root produces a source at
`/workspace/source`, which always works. Unknowns resolve to the safe answer,
the way `sourceNeedsPush` already resolves them.

### 2. Anything else is placed where a source with no host path is placed

Not refused. The colliding path is one **we** chose — the host directory is the
user's, the mount point inside the sandbox is ours — so the fix belongs on our
side of that line:

- the primary source lands at `/workspace/source` (`defaultRunSourceDir`, what
  a remote-URL source has always used);
- a reference lands at `/workspace/<name>` (`referenceRunSourceRoot`, what a
  remote reference has always used), which is what keeps two clamped sources
  from claiming one directory.

The working directory keeps its position *within* the repository rather than
its spelling on this machine, so `discobox run` from a subdirectory still starts
the harness in that subdirectory.

### 3. The Windows mapping becomes a case of this rule, not a parallel one

`windowsRunDestination` already does exactly this: a Windows path with a drive
letter is mirrored under the `/mnt/<drive>/…` name WSL gives it, and one
without — a UNC share, a path inside a WSL distro — falls back to
`/workspace/source` with the subdirectory honored by position. That is this
decision, discovered earlier for a different reason, and it is why `/mnt` is on
the list in §1.

So both platforms ask one question — *what path does this host directory keep
inside a sandbox, if any* — and one placement routine answers it. A rule with
one implementation cannot drift from itself.

### 4. It is decided where the destination is derived: the client

The destination is the client's own reading of a host path the server cannot
see, and neither the server nor the pool agent has ever had an opinion about it.
Both stay as they are.

## Alternatives rejected

**Refuse a source outside those roots.** The stricter reading, and the first
instinct: `discobox run -C /tmp/x` errors and names the allowed roots. Rejected
because it declines work Discobox can do perfectly well. A repository under
`/srv`, `/opt` or `/data` is an ordinary way to keep code on a server, and a
directory under `/tmp` is what every test that creates a discobox from
`t.TempDir()` uses — seven test files in this repository do. None of them is
asking for a path *inside* the sandbox; they are asking for a discobox. The
refusal buys nothing the placement rule does not, and costs the cases where the
only thing wrong was a mount point we picked.

**A forbidden list — `/tmp`, `/var`, `/run`, `/etc`, `/usr`, …** Keeps today's
mirroring for `/srv`, `/opt` and `/data`, which is a real advantage: those
sources keep their own paths. Rejected on the asymmetry in §1. The cost of
wrongly clamping is a source at a different path; the cost of wrongly mirroring
is an empty mount point, a healthy-looking discobox with no code in it, and an
error message about `chdir` that names nothing anyone can act on.

**Disable `tmp.mount` in the sandbox image, or exclude the source path from
tmpfiles.** Makes `/tmp` writable by us. Rejected: it is fighting the operating
system on ground it owns, for a path we chose arbitrarily and can simply not
choose. It also only answers `/tmp` — `/var` and `/etc` are still the image's.

**Have the pool agent or the sandbox-agent relocate an unsafe target.** They are
the layers that get burned, and the sandbox-agent already knows about systemd's
mounts. Rejected because neither knows what the path *meant*: by then the
destination is a string on a create request, with no host directory behind it to
name the source after and no way to tell a deliberate `/opt` from an accidental
`/tmp`. The client is where a host path is still a host path.

**Mount the source before systemd starts, or order the bind after
`local-fs.target`.** Ordering the bind later would make the mount land on the
tmpfs — visible for one boot, gone at the next, which is worse than failing:
the discobox loses its working tree on restart rather than never having had one.

## Consequences

A repository under a root outside §1 — `/tmp`, `/srv`, `/opt`, `/data`, `/var`
— now lands at `/workspace/source` instead of its own path. For `/tmp` that is
strictly better: the path did not exist inside the sandbox at all. For `/srv`,
`/opt` and `/data` it is a real change: a script inside the box that hardcoded
the host path stops matching, and `../foo` between two such sources resolves
through `/workspace` instead. The sources are all still there, under their own
names.

Existing discoboxes keep the destinations they were created with; this decides
where a create puts a source, and a create happens once.

**A client that predates this still sends unsafe destinations**, and the server
and pool agent take them as given (§4). The failure is the one described above,
and it stays silent. Making it loud — the pool agent refusing to bind a source
onto a directory systemd owns — is a separate decision, and a reasonable one;
this ADR does not take it.

`/proc`, `/sys`, `/dev` and `/boot` are clamped like anything else rather than
refused. They hold no repository, so resolving a source there fails before the
destination is ever computed.
