# 26-10-14-129 — A discobox keeps its own Nix daemon socket

- **Status**: Accepted
- **Date**: 2026-10-14
- **Amends**: [0075](0075-the-nix-store-is-a-pool-shared-cache-seeded-on-first-use.md)
  §2 (a third path inside `/nix` is per sandbox, on a different ground than
  profiles and gcroots) and
  [0129](0129-the-sandbox-agent-reads-the-tree-an-export-carries.md) §2 (one
  more excluded entry in the base layer's export table).

## Context

ADR 0075 made `/nix` a pool cache and carved `profiles` and `gcroots` back
out as per-sandbox `data` volumes, so that one sandbox's `nix profile install`
cannot rewrite another's profile. Its premise that every sandbox in a pool runs
the same user was dropped by
[0094](0094-the-pool-cache-is-partitioned-by-the-sandbox-users-uid.md), which
declared `/nix` `"scope": "shared"`: one directory for the whole pool,
whatever uid each sandbox runs as. Everything else under `/nix/var/nix` stayed
in it, including `daemon-socket`.

That directory holds no store state. It holds the address `nix-daemon.socket`
listens on, `/nix/var/nix/daemon-socket/socket`, and every sandbox's systemd
binds that same path at boot. A unix socket path names one listener: the
sandbox that booted last owns the file, and the listeners of every sandbox
booted before it are left with no path. Their `nix` (`NIX_REMOTE=daemon`)
connects to another sandbox's daemon, builds through that sandbox's proxy
environment, and gets `cannot connect to socket at
'/nix/var/nix/daemon-socket/socket': Connection refused` once that sandbox
stops. `systemctl restart nix-daemon.socket` repairs one sandbox by taking the
path from another.

The rest of `/nix/var/nix` was checked for the same mistake. `db`, `gc.lock`,
`temproots`, `userpool`, `builds` and `active-builds` are state every writer of
the store must see, coordinated by locks. `gc-socket` is an address too, but
only a collector holding `gc.lock` binds it, and it exists so every writer of
the store can reach that collector — it belongs on the shared side.
`gc-roots-socket` (`nix store roots-daemon`) would be per sandbox, but the
image installs and enables no unit that binds it. `nix-daemon.socket` is the
only listener the image enables under `/nix`.

## Decision

### 1. `daemon-socket` is a per-sandbox `data` volume

`sandbox-agent/image.json` declares it beside the other two:

```jsonc
{ "path": "/nix/var/nix/daemon-socket", "volume": "data", "excludeFromExport": true, "uid": 0, "gid": 0, "mode": "0755" }
```

The socket keeps the path every nix client already defaults to. Boot binds the
volume after `/nix` (depth order) and before systemd starts, so
`nix-daemon.socket` binds into the sandbox's own directory. The volume
persists, so a restart finds the last boot's socket file; systemd unlinks a
stale socket before binding. `nix-seed` copies nothing into it and its store
copy leaves it out. It is excluded from export: a socket file is nothing to
carry, and the destination binds its own.

### 2. A pool that already ran the old image gets an overlay, and that is accepted

Boot wires a `data` path as an overlay when its target is non-empty after the
parent is mounted (ADR 0007). On a pool whose cache predates this ADR, the
shared `var/nix/daemon-socket` still holds the last old-image sandbox's
`socket`, so the new volume becomes an overlay whose lower layer is that shared
directory. `profiles` and `gcroots` never meet this because `nix-seed` keeps
their cache-side directories empty; this one systemd itself populated.

The overlay still gives the sandbox its own socket: `bind` on the inherited
name fails with `EADDRINUSE`, systemd unlinks it (a whiteout in the sandbox's
upper layer) and binds again, and the new socket lives in the upper layer.
Clients reach that listener, and nothing another sandbox binds in the lower
layer reaches them. The lower can still change underneath the mount while
old-image sandboxes run in the same pool, which overlayfs leaves undefined;
that lasts until those sandboxes leave the old base — for an image built
on an older base, until it is rebuilt — and the name it changes is one the
upper layer already masks.

Nothing removes the stale file from the shared cache. Deleting it would take
the address from whichever old-image sandbox still listens there, and a
nix-specific cleanup in the generic volume wiring is a rule boot does not
otherwise carry.

### 3. Sandboxes on the old image keep sharing until they move

The volume reaches a sandbox only through an image that declares it. Old-image
sandboxes in a pool keep sharing the socket among themselves; each one that
moves to the new image leaves that set.

## Alternatives rejected

- **Move the listener off `/nix`**, e.g. `ListenStream=/run/nix/daemon-socket`
  in the `nix-daemon.socket.d` drop-in and `NIX_DAEMON_SOCKET_PATH` in the
  image env. No persisted socket, no export entry, no overlay case. Rejected
  because only a process that inherits that variable would find the daemon. A
  nix client run as the sandbox user without it looks at the default path,
  finds no socket, falls back to opening the store directly, and fails:
  `opening lock file "/nix/var/nix/db/big-lock": Permission denied`. Today
  such a client — a
  systemd user unit or timer, `systemd-run`, `env -i`, a container started
  with `/nix` bound in — reaches the daemon, because it needs nothing but the
  default path (`env -u NIX_REMOTE nix store info` reports `daemon`). Those
  processes are not one list that can be fixed. Root is no argument either
  way: `sudo` resets `NIX_REMOTE`, and root's `nix` opens the store directly
  under both designs (`sudo nix store info` reports `local`).
- **A per-sandbox tmpfs at the default path**, mounted by a systemd mount unit
  that the socket drop-in requires. It keeps the default path, persists
  nothing, needs no export entry, and is never an overlay, so §2's case does
  not arise. Rejected because it puts a per-sandbox path under `/nix` outside
  the declared-volume model. `image.json`'s volumes — and from them
  `sandbox.json`'s, the answer a sandbox reads for what is its own — would no
  longer list every per-sandbox path under `/nix`. The carve-out would also
  be wired by a unit file rather than by boot, which wires the other two the
  same way on every provider. §2's overlay is accepted as the price of
  keeping one model.
- **A per-sandbox socket name in the shared directory** (`socket-<id>`). Same
  objection — every client has to be told the name — plus a directory of
  stale sockets that grows with the pool.
- **Boot or `nix-seed` deleting the stale shared socket** so the volume is
  always a plain bind. Breaks the old-image sandbox that owns it, and puts a
  nix rule in generic wiring (§2).

## Consequences

- Each sandbox's `nix` reaches its own daemon, with its own proxy environment,
  whatever else boots or stops in the pool.
- `sandbox-agent/DESIGN.md`, `harness/DESIGN.md` and the discobox skill's
  persistence table describe four `/nix` volumes, not three.
- An existing pool runs this volume as an overlay over a directory the pool
  shares (§2) for as long as its cache keeps the old socket file.
