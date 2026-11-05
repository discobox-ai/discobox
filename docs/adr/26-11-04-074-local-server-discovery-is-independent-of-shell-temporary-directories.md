# 26-11-04-074 — Local server discovery is independent of shell temporary directories

- **Status**: Accepted (§1's prohibition on repair narrowed by [26-11-05-240](26-11-05-240-runtime-directory-permission-repair-requires-human-consent.md)) (narrows ADR 0019 §3: an unreachable incumbent may fail startup with a diagnostic; reachable startup and known shutdown handoffs retain their waits)
- **Date**: 2026-11-04

Accepted by the owner by merging #599, 2026-11-04

- **Issue**: [#541](https://github.com/discobox-ai/discobox/issues/541)

## Context

`pkg/endpoint/default_unix.go` locates the local server in `XDG_RUNTIME_DIR`,
falling back to `os.TempDir()/discobox-<uid>/discobox/server.sock`. Nix assigns a
different `TMPDIR` to each development shell. A server started in one shell
therefore cannot be found by another shell of the same user. The CLI launches a
second server, which waits indefinitely on the first server's data-directory
lock in `server/internal/server/singleton.go`. The CLI eventually reports that
its expected endpoint never answered, without explaining where the holder is.

[ADR 0019](0019-one-server-per-data-directory-enforced-by-a-file-lock.md) §3
explicitly rejected failing startup after a deadline: development rebuilds must
wait for the server they replace. That remains correct for a known handoff,
but indefinite waiting cannot recover a server listening at a different address.
The lock's safety guarantee does not require leaving a second process parked.

Three cases constrain the change:

- An older server has no new address or stopping record. In an automatic
  upgrade, `pkg/endpoint/autolaunch.go` requests shutdown and waits for the
  listener to disappear before starting its replacement. Provider cleanup can
  still hold the lock after that disappearance.
- `server/internal/server/startup.go` answers HTTP 503 with a starting health
  status during initialization, including on `/shutdown`. This is a reachable
  server, even though it has not accepted shutdown yet.
- macOS normally gives a user a private `TMPDIR`. Moving its socket into a
  predictable directory under shared `/tmp` without checking ownership lets
  another local user precreate that directory and impersonate the server.

## Decision

### 1. The default Unix socket is stable and private

Keep `XDG_RUNTIME_DIR` when configured. Otherwise use the short, stable path
`/tmp/discobox-<uid>/discobox/server.sock`, independent of `TMPDIR`, `TMP` and
`TEMP`, on Linux and macOS. Explicit server endpoint overrides still select
the configured endpoint. Windows keeps its existing named-pipe default.

Both the server and the client must validate the per-user directories protecting
the default socket before binding or connecting: they must belong to the
current user and deny access to other users. Newly created directories are
private from creation. Validation must not follow a user-controlled symlink or
race an attacker-controlled replacement into another user's directory. A
foreign-owned, accessible-to-others or otherwise unsafe directory is a clear
error; do not repair or take it over, trust its listener, or silently fall back
to another socket. System-owned path aliases such as macOS `/tmp` to
`/private/tmp` are not themselves a reason to reject a safe private directory.

This accepts that another local user can squat on the fallback name and cause
a visible refusal. They must not be able to turn that refusal into a connection
to an impersonated server. The privacy requirement applies to the configured
runtime-directory default as well as the fallback; it does not redefine trust
for explicitly chosen endpoints.

### 2. Record the holder's address beside the data-directory lock

A server holding the lock publishes its PID and configured listen endpoints
in an atomically replaced, private record beside `server.lock`. The record
belongs to that lock's lifetime, is removed before a normal unlock, and is
replaced by the next holder after a crash. Do not delete or recreate the
existing database or change the lock-file format to introduce it.

The record is advisory discovery and diagnostics, never authority to open the
database, bind over an incumbent, signal a PID, or declare a handoff. A stale,
missing or malformed record cannot strand an otherwise free lock or establish
that a live holder is stopping. Windows must support the record without
assuming that its held byte-range lock can be read for a PID.

A recorded endpoint is not permission to shut down a differently configured
server automatically. Keep shutdown requests scoped to the endpoints the
invocation selected; show the holder's other addresses as recovery information.

### 3. Fail clearly for an unreachable holder; preserve startup and handoff waits

This narrows **ADR 0019 §3's rejection of failing fast**, and its consequence
that a wedged incumbent always blocks startup indefinitely. Waiting for the
lock is conditional on what is known about the incumbent:

- **Reachable and starting:** a valid starting health response, including
  HTTP 503, is evidence of reachable initialization. Use the existing readiness
  allowance and report its phase. A refused `/shutdown` alone is not evidence
  that the server is unreachable. A later replacement must still wait for an
  accepted shutdown and the lock's actual release.
- **Known shutdown handoff:** preserve the provider-drain wait after shutdown
  was accepted or the launching CLI observed and initiated the existing upgrade
  handoff. Carry that knowledge through replacement startup, including when
  the older server has no new record and has already closed its listener.
  Do not apply a new short unreachable-holder deadline to that drain. Retain
  the existing readiness/shutdown allowance and caller cancellation, with
  progress naming the lock holder.
- **No reachable incumbent and no known handoff:** allow a bounded discovery
  grace for the holder to publish its record and bind. If it still cannot be
  reached, fail startup with the held lock's path, the holder's PID and
  addresses when available, the endpoint attempted, and an actionable
  `--server <address>` or stop-the-holder hint. Surface this through CLI
  auto-launch as the primary error, rather than the generic `never answered`
  timeout. Do not leave a duplicate child waiting indefinitely after failure.

The discovery grace must fit within the existing initial-answer budget; it
cannot become an unbounded wait by resetting on each probe or metadata read.
A handoff or startup that exceeds its existing allowance reports that specific
state, rather than being mislabeled as an unreachable holder.

For an older holder without a record, report the lock PID where the platform
allows it, explicitly say the address is unavailable, and point to the local
server log for recovery. Do not invent an address or treat missing metadata as
proof of a crash. Diagnostic text and recorded values must be safe to print in
a terminal.

### 4. The singleton safety invariants remain

ADR 0019 §§1, 2 and 4 remain unchanged: the exclusive kernel lock is scoped to
the data directory, taken before opening the database, held until listener and
database cleanup completes, and released last. No timeout kills the holder,
unlocks its file, or permits a second reconciler. Different data directories
remain independent. This decision changes discovery and failure reporting,
not the lock's authority or endpoint unlink semantics.

## Alternatives rejected

- **Keep shell-specific sockets and use only the recorded address.** This
  makes every ordinary connection depend on discovery metadata, and leaves
  old clients unable to locate a server when a shell's temporary directory
  disappears. A stable socket fixes the source of the disagreement; the record
  explains an explicitly configured mismatch and aids recovery.
- **Use an unchecked predictable `/tmp` directory.** That preserves a short
  socket path but loses macOS's private-directory protection. Ownership and
  privacy checks are required on both binding and dialing, not only creation.
- **Fail every lock wait after a short timeout.** This breaks legitimate
  provider drains, particularly upgrades from older binaries, and misclassifies
  a reachable server still starting. The exception is for an unreachable
  incumbent without handoff evidence, not for all contention.
- **Wait indefinitely for every holder, as ADR 0019 §3 requires today.** This
  remains safe for the database but cannot fix differing socket addresses and
  hides the useful error behind the CLI's startup timeout.

## Consequences

A server started inside one Nix development shell is reachable from a plain
shell or another development shell of the same user without a server override.
Socket ownership errors fail closed instead of connecting to another user's
listener. A genuinely unreachable lock holder produces a bounded, actionable
failure while normal initialization and upgrades retain their longer waits.

The address record adds no database migration. A running older server whose
socket lived under another `TMPDIR` cannot acquire the new stable address
retroactively; it may need one explicit shutdown using its logged address.
Known automatic upgrades must still drain it without requiring new metadata.

The record filename/schema, race-safe filesystem operations, discovery-grace
value within the initial-answer budget, and mechanism for carrying observed
handoff evidence are implementation details. They must satisfy the state and
privacy rules above; they are not authorization to weaken them.
