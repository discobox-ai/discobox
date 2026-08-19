# Pool Agent Review Notes

## Ownership of host-backed roots

`prepareSandboxVolumes` provisions the roots that become container binds. The
two helpers are not interchangeable, and the question that picks between them is
*who writes the contents*, not how big they are:

- **`prepareOwnedTree` is for roots this agent materializes end to end** — the
  per-sandbox config and secrets trees, and each source checkout it clones. It
  wrote every byte, one sandbox bounds the size, and the sandbox user must not
  own them, so asserting ownership over the tree is both cheap and meaningful.
- **`prepareOwnedMountpoint` is for everything else** — the sandbox's data root,
  the sources root, and the pool cache. A bind-mount source needs only its own
  ownership to be right; what lives inside belongs to whoever wrote it.

Getting this wrong on the pool cache is a latency bug: it grows without limit
(tens of GB and ~10^6 inodes in normal use), so a recursive chown there cost
~37s of every sandbox create on a cold page cache — and sandbox-agent's
`seedHome` immediately chowned it back, so the two fought over the same inodes
on every start.

Getting it wrong on the **data root is a correctness bug**: that tree is the
sandbox's `$HOME`. Unarchiving is a create against a tree that is already full
(ADR 0022 §6), so a `chown -R 0:0` there took every file the sandbox had ever
written. Do not answer this with "the sandbox chowns it back on boot" —
`seedHome`'s repair is a different component's walk with its own limits (it
stops at home's own filesystem, and does nothing at all for a sandbox that runs
as root or names no user), and a rule that holds only while two passes agree
exactly is not a rule.

Rule of thumb: on the create path, walk only what this agent wrote.

## Sandbox user ids

The pool host cannot resolve a sandbox's account (ADR 0025 §4). An id the
request did not give stays `unsetID` (-1, the chown(2) sentinel) rather than
being guessed — see `chownID`/`chownSpec`.
