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

## Subprocesses

`exec.Cmd.Run`, `Output`, `CombinedOutput` and `Start` are not for this module:
the agent is PID 1 and reaps orphans, so a child it does not know it owns is one
the reaper may collect out from under `Wait`. Start subprocesses through
`childproc` (ADR 0087). What a stolen status looks like at the call site is
`waitid: no child processes` over complete output and a command that ran.

Do not turn a command's failure into a fact about the world — "get-url failed,
so there is no remote", "the check exited nonzero, so it is not there". Ask for
the value and read the answer, and prefer one write that both creates and
corrects over deciding between add and update.

## Error responses

A status this API reuses for two conditions needs a `type` to tell them apart.
409 is both "already exists" and "archived", and the control plane acts on them
in opposite ways, so `mapRuntimeError` stamps the archived one with
`model.ErrorTypeSandboxArchived` and `NewError` copies it onto the response. Do
not let a caller's only signal be the detail string: it is prose, it is written
for people, and nothing stops it being reworded.
