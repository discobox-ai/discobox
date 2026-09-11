# 0107 — Homebrew is image content on an overlay, handed to a group

- **Status**: Accepted (adds a rule to
  [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) §3's
  mount list)
- **Date**: 2026-09-11

## Context

The sandbox image ships a package manager per language and Nix for everything
else. Homebrew is the one general-purpose manager it does not have, and the
formula an agent reaches for is routinely a `brew install` away in the
instructions it is following.

Adding it is not an apt line. Three facts constrain the design, and each one
rules out the arrangement that would otherwise be obvious.

**The prefix is not a preference.** Homebrew builds its Linux bottles for
`/home/linuxbrew/.linuxbrew` and relocates nothing. Any other prefix is legal
and makes every formula a source build, which turns a ten-second install into a
compile of the dependency closure. So the prefix is fixed, and everything below
follows from having to make *that path* work.

**The uid is not known at build time.** A sandbox's user comes from the manifest
and is resolved inside the box (ADR 0025, ADR 0033). The image cannot chown the
prefix to the account that will use it, because that account does not exist yet
and differs between sandboxes built from the same image.

**Nothing installed at runtime survives the container.** A `brew install` writes
into the container's writable layer and is gone when the sandbox is recreated —
the same problem ADR 0075 solved for `/nix`.

## Decision

### 1. The prefix is baked into the image and declared a `data` volume

The Dockerfile clones `Homebrew/brew` at a pinned tag into
`/home/linuxbrew/.linuxbrew`, and `sandbox-agent/image.json` declares:

```jsonc
{ "path": "/home/linuxbrew/.linuxbrew", "volume": "data" }
```

ADR 0007 §3 already says what that means: a `data` path whose target ships
content is wired as an overlay, image tree as the lower layer, writes persisting
to the sandbox's data volume. The image's brew is visible, a sandbox's own
installs outlive a stop/start, and nothing is copied at boot.

**This is deliberately not what `/nix` does**, and the difference is worth
stating because the two look like the same problem. ADR 0075's seed unit,
stamps, `flock` and PATH shims exist because the nix store had to be a
*pool-shared cache* volume — shared across concurrently running sandboxes, so
never an overlay (0007's third rejected alternative) — and a cache path is
always a plain bind, which would hide whatever the image shipped underneath it.
Hence the store moved aside to a seed directory and copied back on first use.

Brew has no such requirement. Its Cellar is per-sandbox state, `data` is the
right backing, and a data path gets the overlay for free. Adopting the nix
machinery here would be a seed unit, two stamps and a set of shims bought for
nothing.

The cost accepted: the Cellar is per-sandbox, so two sandboxes on a pool each
unpack their own copy of a bottle. Only the *downloads* are shared, via
`HOMEBREW_CACHE` under `~/.cache`, which is a uid-partitioned pool cache
(ADR 0094) — shared with same-uid sandboxes on the pool, not pool-wide.

### 2. The tree is handed to a group, not to a uid

The image creates a `brew` system group, and leaves the prefix `root:brew`,
group-writable, with setgid directories so anything installed later keeps the
group. `image.json` lists `brew` in `additionalGroups`, so boot adds whatever
uid the sandbox resolves to that group alongside `docker` and `kvm`.

The alternative is a recursive chown at boot, once the uid is known. It is
rejected because of what a chown costs *on an overlay*: changing an inode's
owner forces a copy-up, so chowning the tree would copy the entire image-shipped
prefix into the sandbox's data volume on every first boot — paying the full
price of the seed this design exists to avoid, and paying it for a sandbox that
never runs `brew`.

Nothing outside the image names the group's gid: `groupadd --system` picks an
unused one at build time and boot looks the group up by name. The gid is
deliberately *not* claimed to be clear of the range a manifest's gid occupies —
a manifest gid is the client's own local gid, which on macOS is `staff`'s 20.
A collision is harmless: boot's `ensureGroup` would find `brew` by gid and make
it the account's primary group, which still leaves the user in it.

### 3. An overlay's upperdir adopts the target's ownership and mode

ADR 0007 §3 lists the overlay rule as "overlay mount, then apply uid/gid/mode".
A step is missing from that list, and §2 above is what makes it visible.

**overlayfs reports the upperdir's ownership and mode for the merged root**, not
the lower's. `wireVolume` creates the upperdir with `os.MkdirAll(dir, 0o755)` as
root, so the merged root presents as `root:root 0755` however the image built
the path. The declaration's `uid`/`gid`/`mode` would correct it — but a path
that is already right in the image has no reason to state them, and the brew
prefix is exactly that path.

So the boot flow gives the upperdir the target's own ownership and permission
bits — setgid included — before mounting. `applyOwnership` still runs after, so
a path that does declare `uid`/`gid`/`mode` ends up precisely where it did
before; the new rule only speaks where the declaration is silent.

The failure it prevents is a confusing one, which is the argument for fixing the
rule rather than the path. Only the *top level* misbehaves: everything beneath
inherits the lower directory's attributes on copy-up, so a group-writable tree
accepts writes everywhere except its own root. For brew that is `brew install`
failing to create `Cellar`, `opt`, `lib` and the rest on first use, while every
subsequent write into them would have been fine.

One path already takes the overlay branch: `%HOME%` on a root sandbox, where
the image's `/root` ships a shell skeleton and rather more that later layers put
there. Its behavior is unchanged, and the reason is the ordering above rather
than the path being new — `%HOME%` declares `uid`/`gid`/`mode`, so
`applyOwnership` runs after and overwrites whatever this step set. The brew
prefix is the first path to *depend* on the new step, not the first to go
through it, and that distinction is the thing to preserve: every root sandbox's
home runs through this code on every boot, and what makes that safe is
`adoptDirIdentity` coming before `applyOwnership` rather than after.

### 4. Root sandboxes get brew, and that is accepted

A manifest that names no user runs as the image's own account (ADR 0025 §5),
which is root. Homebrew's documented position is that it refuses to run as root
— but its own guard stands down when it sees `/.dockerenv`, `/run/.containerenv`
or a container cgroup, on the grounds that root is normal there. Every sandbox
is a container, so the refusal never fires.

Root therefore gets a working brew that installs root-owned kegs into the
overlay, and §2's group arrangement is simply moot for it. Nothing gates this.
Gating it would mean re-imposing a restriction upstream deliberately lifted for
exactly this environment.

## Consequences

- `brew` is on PATH in every sandbox, at the prefix its bottles are built for,
  and a sandbox's installs survive a stop/start.
- The image grows by the brew repository plus the vendored ruby it would
  otherwise fetch on first use — the ruby is baked because, left to first use,
  every sandbox would fetch it *and* copy it up into its own overlay.
- `HOMEBREW_NO_AUTO_UPDATE` is set, because brew's pre-install `git pull` of its
  own repository would copy that repository up into the sandbox's volume.
  Formula data still refreshes; it comes from the JSON API, which this does not
  touch. `brew update` is therefore not how a sandbox gets current.
- **Brew goes after `/usr/local/bin` on PATH**, not before. That is where the
  image installs shims that are required rather than convenient — `docker`
  (ADR 0044 §8), whose absence makes a nested `docker build` silently run on the
  sandbox's own dockerd, and the nix shims (ADR 0075 §4). `brew install docker`
  installs a real Docker CLI, and ahead of `/usr/local/bin` it would replace
  that shim permanently. Brew still precedes `/usr/bin`, which is the point of
  having it. The hazard is not eliminated — `~/.local/bin`, `~/.cargo/bin` and
  the nix profile all still precede `/usr/local/bin`, so a `docker` CLI
  installed through any of them disarms ADR 0044 the same way.
- A bottle-less formula builds from source into the overlay. `build-essential`
  is present, but this path is untested here.
- Two same-uid sandboxes share `~/.cache/Homebrew/downloads` while brew's locks
  live under the per-sandbox prefix. Concurrent installs of the same formula are
  not known to be safe, and nothing in this design makes them so.

## Alternatives rejected

**Install brew under `$HOME`.** The home directory is already a per-sandbox data
volume, so persistence and ownership would both be free — no group, no overlay
rule, no image layer. Rejected because Homebrew ships bottles only for
`/home/linuxbrew/.linuxbrew`: every formula would build from source, which is
the difference between `brew install` being useful and being a trap.

**Seed the prefix on first use, like `/nix`.** Ship it aside at
`/usr/local/lib/discobox/brew` and copy it into an empty prefix under a lock on
first `brew`. Rejected: the machinery exists to work around a *cache* volume's
plain bind, and brew's prefix is per-sandbox `data`, where the overlay does the
same job with no unit, no stamp and no shim. It would also be strictly worse —
a first-use copy of the whole prefix is the cost the overlay avoids entirely.

**Chown the prefix at boot instead of using a group.** The straightforward
reading of "the uid is not known until boot". Rejected in §2: a chown across an
overlay copies every inode up, so it pays the seed's cost on every first boot,
for every sandbox, whether or not brew is ever used.

**Pin a fixed numeric gid and declare it in `image.json`.** Would let the volume
declaration state `gid`/`mode` and correct the merged root through the existing
`applyOwnership` path, with no change to `wireVolume`. Rejected on three counts,
the last of which is decisive. It puts a magic number in two files to work
around a general defect — every overlayed path loses the image's ownership, not
just this one. A fixed gid is likelier to collide with a manifest's than one
`groupadd --system` picks. And when this was written it did not work at all:
`ResolveVolumes` parsed the declared octal and cast it straight to
`os.FileMode`, which carries setuid/setgid/sticky far above their POSIX
positions, so a declared `"2775"` reached `os.Chmod` as `0775` — group-writable
with no setgid, no error, and every directory `brew install` created landing in
the wrong group. That conversion is fixed as part of this change, so a future
image declaring setgid gets it; the first two reasons are why this design still
does not.

**Make the Cellar a pool cache so bottles unpack once per pool.** Rejected for
now: brew's locks are under the prefix, and a shared Cellar with per-sandbox
`bin`/`opt` symlink trees would desync. Revisit if per-sandbox unpacking proves
expensive enough to matter.
