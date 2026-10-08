# Sandbox Agent Review Notes

## Run identity

Getting "which user does this run as" wrong is the most repeated mistake in this
module. It fails quietly — the process starts, and only some capability is
missing — so it survives review easily. Rules, and why each exists:

- **Resolve through [`runuser`](runuser/DESIGN.md), never by hand.** One call:
  `runuser.Resolve(layers, need)`, with the image, manifest and request layers
  and the fields the caller needs. Precedence and completion belong to it and
  to `sandboxuser.Merge`. `DISCOBOX_USER_*` (boot's `manifestUser`) and
  `config.ExecDefaults` (the server's `execDefaultUser`) are read once each, as
  the manifest layer. Treating either as the resolved user is a second
  construction of the same identity, and the two always drift. That drift is
  exactly how terminals came to run without the sandbox's supplementary groups
  while plain execs kept them.
- **Outside boot, ask the exec manager.** `execs.Manager.ResolveUser(req)`
  supplies the three layers and does no merging of its own. `terminal`, the
  server and `ports` all go through it. Do not assemble layers yourself, and do
  not rebuild the default user.
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
- **A group the image never created is skipped, not fatal.** `runuser.Groups`
  drops it from the credential, the same way boot's `ensureAdditionalGroups`
  skips it, so the two cannot disagree about the same image. A harness Dockerfile
  that forgot to install a package must not break every process in the sandbox.

Decision records: [ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
[ADR 0033](../docs/adr/0033-user-resolution-is-one-layered-resolver-with-declared-gaps.md).

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
- Identity resolution is per platform only behind `runuser`'s seam
  (`platform_linux.go`, `platform_other.go`), and both resolvers build
  everywhere so the one-account one is tested on Linux; keep identity logic out
  of `_unix.go` files, because darwin is unix and has no POSIX ids here. Only
  the credential and `SysProcAttr` construction are otherwise per platform
  (`execs/credential_linux.go`, `execs/credential_darwin.go`,
  `execs/process_unix.go`, `execs/process_windows.go`). Run
  `go tool task check:windows` before relying on that split.
- Off Linux, a request naming another user, a uid or a group set is refused
  with `sandboxuser.OneAccountError`, never dropped. Do not "tolerate" one by
  clearing the fields: that runs the exec as the one account while its record
  says it asked for someone else (ADR 0145 §5).
- A signal a platform cannot deliver as asked is mapped to its nearest real
  mechanism and recorded as an exec event. A new mapping goes in `procio`'s
  delivery tables with a `Reason`; one with no reason is a signal delivered as
  itself.

## Supervision

- **The supervisor's lifetime lock is `flock`, never `fcntl`.** A flock
  belongs to the open file description, so it travels to the shim with the
  descriptor and outlives the supervisor closing its own copy; a POSIX record
  lock belongs to the process and would be released the moment it did.
- **Only the shim takes the lock exclusively.** Every probe and wait in the
  agent takes it shared. An agent-side exclusive lock, however brief, reads
  as a live shim to a concurrent probe, and `Stop` then signals a pid that
  may already be someone else's.
- **The shim marks the lifetime descriptor close-on-exec before it starts
  anything** (`execs.HoldLifetime`). A command that inherited it would hold
  the lock past the shim's exit, and the exec would read as running for as
  long as anything it started did.
- **A stop ends the command's session, not its process group.** An
  interactive shell gives every job its own group; a group kill leaves a
  terminal's background jobs running after the exec reads stopped.
- **Never signal a session without asking whether it is still the
  command's** (`signalSession`, `endSession`). Every exec command leads a
  session, so "leads a session" does not identify one: a live leader must
  have the kernel identity the shim recorded (`Exec.ProcessIdentity`), and
  compared exactly. A wall-clock start within a tolerance is not an identity:
  a process that reused the number within it passes.
- **A lock that cannot be waited on is not a shim that ended.** Reading an
  error as "gone" demotes the exec to lost, and the terminal layer relaunches
  a lost terminal over the shim still running.
- **A unit manager is required and complete.** Add behavior to `UnitManager`
  and to every implementation, including the test fakes; run the shared
  contract (`testUnitManagerContract`) against each.

## Reading the working tree at boot

- **Anything at boot that reads the sandbox's sources goes behind
  `agentRuntime.awaitSources`.** In a push-delivered sandbox the container is
  running before its working tree exists (ADR 0001), so boot code that reads it
  is racing the delivery — and the race is silent, because an empty tree is
  indistinguishable from a repository that declares nothing. That is exactly how
  `.discobox/services` came to start on no sandbox at all: discovery ran ~3
  seconds early, found no directory, returned no error, logged nothing, and
  never looked again.
- **One gate, shared.** Take the field; do not call `sourcesready.Gate` a second
  time. Two constructions of the same wait is how one of them ends up gating
  nothing.
- **A one-shot at boot needs a reason it will not be re-run.** `EnsureStarted`
  runs once. Anything else with that shape has to be either gated or repeated —
  "it will be picked up later" is only true if something looks later.

## Boot cost

Boot runs before anything in the sandbox is usable, so work here is latency the
user waits on every single start.

- **Never let a recursive walk under `$HOME` cross into a mounted volume.** The
  shared pool cache and the source trees are mounted *under* home
  (`~/.cache`, `~/go/pkg/mod`, `~/.local/share/pnpm`, the source targets). They
  are unbounded — ~4.7*10^5 inodes on a working machine — and they already have
  the ownership `wireVolume`/`wireSources` and the pool agent gave them. Walking
  them cost ~14s of every boot on a cold page cache. `seedHome` uses
  `chownTreeOnOwnFilesystem` for exactly this reason; GNU `chown` has no
  `--one-file-system`, so reaching for `chown -R` reintroduces the bug.
- **What the recursion is actually for** is the intermediate directories boot
  creates as root on the way to a mountpoint — `~/.cargo` above
  `~/.cargo/registry`, `~/go/pkg` above `~/go/pkg/mod` — because
  `applyOwnership` chowns only the mountpoint. Those live on home's own
  filesystem, so the same-filesystem walk still covers them.
- **An overlay's upperdir adopts the target's identity before
  `applyOwnership`, never after.** overlayfs presents the upperdir's owner and
  mode as the merged root's, so `wireVolume` runs `adoptDirIdentity` (setgid
  included) before mounting. Then `applyOwnership` applies only what the path
  declares (ADR 0107 §3). Reverse the order and a declared uid/gid/mode is
  overwritten. Drop the adopt and every overlayed path shows up as root:root
  0755, which refuses writes at its top level only.
- **Ownership has one owner per path.** If the pool agent already owns a tree,
  boot must not assert it again. Both sides asserting produced two full walks of
  the same inodes, in opposite directions, on every start.

## Platform observation

- **Ports, resource counters and power-off go through their seam**
  (`ports.Scanner`, `resources.Sampler`, `autostop`'s `platformPowerOff`). A
  new `/proc`, cgroup or `systemctl` read outside the Linux implementation is
  a Linux-only answer that breaks on darwin without failing a Linux test.
- **Keep the tagged files to the system calls.** Parse what a platform's tool
  prints in an untagged file, so the parser is tested in every CI lane rather
  than only on a Mac.

## Idle stop

- **Every client connection this process serves must hold `autostop`** —
  exec attach, one-shot attach, `tcp/attach` and `udp/attach` today — for as
  long as the client is connected. The shims' attacher counts are not enough: a tunnel has
  no shim at all, and a shim's record of access ends with its exec, so a client
  that just finished a long command would count for nothing. Forgetting is
  silent until the sandbox powers off under someone (ADR 0108 §2).
- **Reading is never activity.** A status poll, a listing, a title re-sent
  unchanged: none of them may move the idle clock. The pool agent polls every
  sandbox's status every 15 seconds, so anything a read counts as activity
  keeps every sandbox up forever.
- **Output is not activity; a changed screen is.** `screenBuffer.sumText`
  hashes cell content only. Adding styles, the cursor, the scrollback (filled
  on every `CSI 2J`), or raw byte counts to it makes a self-drawn blinking
  cursor or an idle TUI's redraw hold the sandbox up forever. So does
  comparing on a write rather than when a burst ends: a frame split across PTY
  reads is a different screen part-way through. A burst settled by a
  `/status` read is dated to its last write, never to the read (ADR 0124).
