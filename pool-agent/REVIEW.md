# Pool Agent Review Notes

## Ownership of host-backed roots

`prepareSandboxVolumes` provisions the roots that become container binds. The
two helpers are not interchangeable, and picking the wrong one is a latency bug
that only shows up on a machine with a warm working set:

- **`prepareOwnedTree` is for per-sandbox roots only** — data, config, sources,
  secrets. One sandbox bounds their size, and this agent materializes their
  contents, so asserting ownership over the tree is both cheap and meaningful.
- **`prepareOwnedMountpoint` is for anything shared or unbounded** — the pool
  cache above all. A bind-mount source needs only its own ownership to be right;
  what lives inside belongs to whoever wrote it. The pool cache grows without
  limit (tens of GB and ~10^6 inodes in normal use), so a recursive chown there
  cost ~37s of every sandbox create on a cold page cache — and sandbox-agent's
  `seedHome` immediately chowned it back, so the two fought over the same inodes
  on every start.

Rule of thumb: if the path is shared between sandboxes, or its size is not
bounded by one sandbox, never walk it on the create path.

## Sandbox user ids

The pool host cannot resolve a sandbox's account (ADR 0025 §4). An id the
request did not give stays `unsetID` (-1, the chown(2) sentinel) rather than
being guessed — see `chownID`/`chownSpec`.
