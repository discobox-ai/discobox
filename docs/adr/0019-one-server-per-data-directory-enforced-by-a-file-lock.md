# 0019 — One server per data directory, enforced by an advisory file lock

- **Status**: Proposed
- **Date**: 2026-07-30

## Context

Two `discobox-server` processes ran against the same SQLite database for over an
hour on a developer machine. Both reconciled the same pool, each computing its
own `dockerworker` config revision, and each saw the other's container labels as
drift. The pool container was force-removed and recreated roughly twice a
minute, indefinitely. Every recreate took the pool's MITM proxy down with it, so
sandboxes saw `dial tcp: connect: connection refused` and the coding harness
inside them saw `API Error: Connection closed mid-response`. Nothing anywhere
reported that two servers were running.

The duplicate was not supposed to be possible. ADR-less commit `50ecbb39`
established the intended protocol: on startup, ask whoever holds the listen
endpoints to shut down, then retry the bind until it succeeds, so a server
spawned during an `air` rebuild window cannot displace a running one. Its
enforcement was the kernel refusing the second bind — the commit message names
the symptom it fixed as "address already in use on the TCP endpoint".

Two later, individually correct changes removed that enforcement:

- `localipc.Listen` unlinks a unix socket path before binding it. A unix bind
  therefore never returns `EADDRINUSE`; the newcomer always rebinds the path.
- The default listen set became local IPC only. A TCP listener is opted into,
  never implied, so most servers have no endpoint that can report `EADDRINUSE`.

The retry loop in `listenWithReclaim` is consequently dead code in the default
configuration, leaving only a best-effort `/shutdown` request with a 2-second
timeout whose result is discarded. When the incumbent misses that window, the
newcomer unlinks, binds, and both keep serving.

The resulting state is not self-correcting. `/shutdown` is addressed by socket
path, and the displaced server no longer owns that path — it holds a listener on
an orphaned inode. No future server can ever ask it to leave. Observed: the
incumbent survived 85 minutes and several rebuilds, and had to be killed by
hand.

## Decision

### 1. The singleton boundary is the data directory, not the listen endpoint

The server takes an exclusive advisory lock on `<data dir>/server.lock` and
holds it for its lifetime. The lock is acquired in `Run` before the database is
opened.

The database is the resource duplicates actually corrupt each other over. Two
servers sharing one database reconcile the same resources against each other
whether or not they share an endpoint; two servers on separate data directories
share nothing and are legitimately independent.

Scoping to the endpoint instead was rejected: it is the narrower invariant and
misses a real case. A second server started with a different
`DISCOBOX_SERVER_LISTEN` — a developer adding a TCP endpoint, a stray daemon
launched with an override — binds a different path, passes an endpoint-scoped
check, and proceeds to reconcile against the same database. That is precisely
the failure this ADR exists to prevent, so the check has to sit at the database.

### 2. Enforcement is a lock, because binding proves nothing

Binding an endpoint cannot answer "is another server running?" while
`localipc.Listen` unlinks first, and adding a TCP endpoint purely to regain
`EADDRINUSE` would make a machine-wide network surface a correctness dependency.

Making `localipc.Listen` refuse to unlink was rejected. It would restore
`EADDRINUSE` for unix sockets, but a unix socket file is not removed when its
owner dies: any crash would leave a file that blocks every subsequent start
until a human deletes it. That trades a silent duplicate for a hard start
failure after every crash.

A PID file was rejected for the same class of reason: it goes stale exactly when
it matters, after `SIGKILL`, and validating it means guessing whether a live PID
is the same process that wrote it.

An advisory file lock has neither problem. The kernel releases it when the
holder dies, however it dies, so a crashed server cannot strand it, and a live
lock is proof of a live holder with no liveness check to get wrong.

### 3. A starting server asks the incumbent to leave, then waits — it never displaces and never gives up

`50ecbb39`'s handshake is kept and made load-bearing. The newcomer requests
`/shutdown` on every pass, then waits on the lock, logging the holder's PID each
time. It does not proceed until the lock is genuinely free.

Killing the incumbent after a timeout was rejected. A server that is merely slow
to drain — a long attach stream, a slow reconcile — is indistinguishable from a
wedged one from the outside, and the escalation would make the server able to
kill processes it did not spawn.

Failing fast after a deadline was also rejected. In the `air` workflow the
newcomer is the one the developer wants, and exiting non-zero would leave the
stale server running, which is the state this ADR is trying to eliminate.

Waiting forever is only acceptable because it is loud: the wait is logged on
every pass and names the holding PID, so an incumbent that will not leave is
visible rather than silently duplicated. Silence is what made the original bug
survive.

### 4. The lock is acquired first so it is released last

`Run` acquires the lock before opening the database and defers its release
immediately, so the release runs after the deferred listener cleanup that
removes the socket file.

The reverse order has a real race: the waiter could take the lock and bind a
fresh socket while the outgoing server's deferred cleanup was still about to
`os.Remove` that path, leaving a running server that nothing can reach.

## Consequences

**Consequence: `localipc.Listen`'s unlink-before-bind stays as it is.** It is
safe once only one server can reach it, and the alternative strands socket files
after crashes. The `EADDRINUSE` retry in `listenWithReclaim` also stays: it is
dead code for unix endpoints but still correct for a TCP endpoint held by a
foreign process, which the data-directory lock says nothing about.

**Consequence: the lock binds only servers that implement it.** A process
started from an older binary does not participate and can still coexist. The
duplicate that prompted this ADR had to be killed by hand for exactly this
reason. This is inherent to introducing a lock and needs no mitigation beyond
restarting servers once.

**Consequence: two servers on separate data directories remain legal.** This is
deliberate — they share no state. It also means the lock does not protect a
shared *remote* database, where two servers have different data directories and
the same tables. Nothing in the system runs that way today; if it ever does, the
boundary has to move into the database rather than the filesystem, and this
decision should be revisited rather than extended.

**Consequence: a wedged incumbent blocks startup indefinitely.** By design, and
logged each pass. The operator's escape is to kill the holder, whose PID is in
both the log line and the lock file. The lock file's PID is advisory — written
after acquisition, read without the lock — and must never be used for anything
but that message.

**Consequence: Windows needs a second implementation.** `flock` and
`LockFileEx` differ enough to need separate files, so the platform split is a
`lockFileNB`/`unlockFile` pair normalizing "already held" to one sentinel error.
This makes `golang.org/x/sys` a direct dependency of the server module.

## References

- `50ecbb39` — the shutdown-and-reclaim handshake this restores the enforcement
  for.
- `server/DESIGN.md`, "Single Server Per Data Directory" — current state.
