# Sandbox Agent Review Notes

## Run identity

Getting "which user does this run as" wrong is the most repeated mistake in this
module. It fails quietly — the process starts, and only some capability is
missing — so it survives review easily. Rules, and why each exists:

- **Resolve through [`runuser`](runuser/DESIGN.md), never by hand.** One call:
  `runuser.Resolve(User)`. Reading `config.ExecDefaults` or `DISCOBOX_USER_*`
  into a `User` yourself is a second construction of the same identity, and the
  two always drift. That drift is exactly how terminals came to run without the
  sandbox's supplementary groups while plain execs kept them.
- **Inside `execs`/`terminal`, ask the manager.** `Manager.ResolveUser(req)`
  applies the request-vs-manifest and group rules first, then resolves. Do not
  call `runuser.Resolve` directly there and do not rebuild the default user.
- **Never invent an id.** No `uid = 0`, no `gid = uid`, no `uid = 1000` for a
  bare name. UIDs and GIDs are separate namespaces; `uid == gid` is a `useradd`
  default, not a rule. A missing id is read from the passwd entry, and a uid with
  no entry is an error.
- **Never fall back after a failed resolve.** Returning the error is the point;
  substituting a default reintroduces the guess.
- **Groups are all-or-nothing.** A request naming none inherits the sandbox's; a
  request naming any uses exactly those. Never union the two — merging makes the
  manifest a floor no caller can get under, so nothing can ever run with fewer
  groups than its sandbox.
- **A request chooses identity and membership independently.** Naming a user
  must not change groups, and naming groups must not change the user.
- **Group names resolve only in the sandbox.** `/etc/group` lives in the image.
  Code running on the pool host or in the control plane cannot resolve a name and
  must not guess a number for it; it leaves the value unset (`-1` for a chown)
  and lets the sandbox decide.
- **A group the image never created is skipped, not fatal.** Mirrors the boot
  flow, so the two cannot disagree about the same image. A harness Dockerfile
  that forgot to install a package must not break every process in the sandbox.

Decision record: [ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md).

## Testing identity

- Use `runuser.FixedDatabase()` with `t.Cleanup`. Never resolve against the
  machine's real accounts — `osuser.Current()` makes a test pass or fail on
  whoever runs it, and hides id assumptions behind whatever the host happens to
  use.
- Do not write fixtures where `uid == gid`. That coincidence is what the rules
  must not rely on, so reproducing it hides the bug.

## Processes

- Credentials must set supplementary groups explicitly. `NoSetGroups` leaves the
  child holding the *agent's* groups — the agent is root — so a process dropped
  to the sandbox user silently inherits root's groups and none of its own.
- Identity resolution is cross-platform; keep it out of `_unix.go` files. Only
  the credential and `SysProcAttr` construction are platform-specific. Build with
  `GOOS=windows` before relying on that split.

## Boot cost

Boot runs before anything in the sandbox is usable, so work here is latency the
user waits on every single start.

- **Never let a recursive walk under `$HOME` cross into a mounted volume.** The
  shared pool cache and the source trees are mounted *under* home
  (`~/.cache`, `~/go/pkg/mod`, `~/.local/share/pnpm`, the source targets). They
  are unbounded — ~5*10^5 inodes on a working machine — and they already have
  the ownership `wireVolume`/`wireSources` and the pool agent gave them. Walking
  them cost ~14s of every boot on a cold page cache. `seedHome` uses
  `chownTreeOnOwnFilesystem` for exactly this reason; GNU `chown` has no
  `--one-file-system`, so reaching for `chown -R` reintroduces the bug.
- **What the recursion is actually for** is the intermediate directories boot
  creates as root on the way to a mountpoint — `~/.cargo` above
  `~/.cargo/registry`, `~/go/pkg` above `~/go/pkg/mod` — because
  `applyOwnership` chowns only the mountpoint. Those live on home's own
  filesystem, so the same-filesystem walk still covers them.
- **Ownership has one owner per path.** If the pool agent already owns a tree,
  boot must not assert it again. Both sides asserting produced two full walks of
  the same inodes, in opposite directions, on every start.
