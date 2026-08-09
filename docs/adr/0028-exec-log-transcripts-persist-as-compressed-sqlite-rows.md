# 0028 — Exec/terminal transcripts persist as compressed sqlite rows, not tmpfs jsonl files

- **Status**: Accepted
- **Date**: 2026-07-31

## Context

Every exec's (including harness terminals') full stdin/stdout/stderr
transcript was written by `sandbox-agent/execs.AsyncLogger` as base64-JSON
lines, bucketed into one file per 15 seconds, under
`/run/discobox/.../execs/logs/<execID>/<bucket>.jsonl`. `/run` is a tmpfs
mounted with `size=64m` for the *entire* worker/pool container
(`server/providers/dockerworker/engine.go`), shared by every sandbox running
in it — exec sockets, runtime status, secrets, proxy CA bundles, and every
terminal's transcript all compete for the same 64MB. Nothing ever pruned
these files, so a long or verbose session could exhaust that budget, and
every transcript was lost on any pool container restart, since tmpfs does not
survive one.

A sqlite/GORM store already exists in the same binary
(`sandbox-agent/store`, DB at `/var/lib/discobox/sandbox-agent.db`) and is
durable: that path is bind-mounted to host storage per-sandbox
(`layout.go`), unlike `/run`. It held only small metadata rows — exec
state/events, resource snapshots, harness hook payloads — never transcript
bytes, despite living right next to the tmpfs logs in the same process.

## Decision

Exec transcripts now persist as rows in that sqlite store
(`store.ExecLogChunk`), not as files anywhere. `AsyncLogger` still buffers an
exec's output in memory per ~15-second bucket exactly as before, but instead
of appending each entry to a file, it flushes a bucket — as one zstd-compressed
blob, one `INSERT` — when **either** the 15-second timer elapses, **or**
buffered raw bytes cross a 256KB threshold, **or** the logger is closed
(final, usually-partial bucket). Reads (`disco sandbox exec logs`, and the
CLI's post-exit output replay) decompress and concatenate every chunk for an
exec, oldest first — the same shape the file-glob reader used to produce.

Chunks older than a fixed 14-day retention window are pruned in the same
transaction as each insert (mirrors the existing `ResourceSnapshot`
insert+prune pattern), and an exec's chunks are hard-deleted when the exec
itself is deleted (`Manager.Delete` previously never cleaned up transcripts
at all — this closes that pre-existing gap too).

### Batching, not one write per chunk

The exec's PTY/pipe pump reads output in 32KB chunks continuously. Writing
one sqlite row per chunk would turn a verbose terminal into a stream of tiny
transactions against the same database that also serves exec state
lookups — that would be the real "abusing sqlite" failure mode, not sqlite
itself. Batching by time-or-size keeps write volume to roughly one row per
active terminal per 15 seconds (or per 256KB of output, whichever comes
first), which the store's existing WAL configuration
(`journal_mode(WAL)`, `busy_timeout(5000)`, `synchronous(NORMAL)` —
`gormdb/db.go`) handles without any additional tuning.

### Cross-process writer

Each exec runs as its own OS process (`systemd-run` executing
`discobox-sandbox-agent exec-shim`, see `execs/systemd.go`), separate from
the main sandbox-agent server process that owns the long-lived `*store.Store`.
The exec-shim process now opens its own connection to the same sqlite file
(`--database <path>`, threaded through `StartRequest`) before constructing
its `AsyncLogger`. sqlite's WAL mode already supports multiple writer
processes, serialized via `busy_timeout`; no locking scheme beyond what
`gormdb` already configures was needed.

### Bounded read staleness is accepted

A read of a transcript while its exec is *still running* (`disco sandbox
exec logs <id>` on a live session) can lag by up to the flush interval,
since unflushed buffered output isn't visible to a separate reader process
until it's written. This is accepted rather than engineered around: this log
path already only backs the forensic `terminal logs` command
(`sandbox-agent/DESIGN.md`) — live attach/reconnect is served by a separate
in-memory ring buffer (`shimruntime`'s `Replayer`) that this change does not
touch. The far more common read — the CLI's `sandboxExecOutput`, used only
after `waitSandboxExecExit` confirms the exec has exited — is unaffected: a
documented invariant (`shim.go`, `wait()`) already withholds the `Exited`
status until the logger has fully drained, and that invariant now gates the
final DB flush the same way it gated the final file write.

## Alternatives rejected

**Durable plain files (move the jsonl tree from `/run` to the already
bind-mounted `/var/lib/discobox`, keep the file format).** This would have
fixed durability and the tmpfs pressure with a much smaller diff. Rejected
because the actual ask was for exec transcripts to live in "one durable
artifact, not a pile of loose files" — a single sqlite database is simpler to
reason about and to back up than a database plus a per-exec directory tree
that also needs its own retention/rotation logic. Given that requirement, the
extra design cost of batching into sqlite over files is small.

**One row per 32KB output chunk, uncompressed.** This is the design that
actually deserves the "abusing sqlite" label — many small transactions per
second per active terminal against a database also used for frequent exec
state reads. Rejected in favor of the time-or-size batching described above.

**Brotli instead of zstd.** Brotli's dictionary and higher compression
levels suit static, one-shot text (its usual use case is web assets served
once); zstd is the more idiomatic choice for append-oriented, streaming
text/log data (the same reasoning behind journald, Loki, and Kafka's choice
of zstd for log/message compression) and was already indirectly present in
this repo's dependency graph. The ratio difference on terminal output
(mostly repetitive ANSI/text either way) is not large enough to outweigh
that fit.

**A live-tail RPC to eliminate read staleness entirely** (the exec-shim
exposing "give me the current unflushed buffer" over its existing attach
Unix socket, merged with committed rows at read time). Deferred: this log
path is documented as forensic-only, not a live-viewing path, so the
complexity of a new cross-process query wasn't justified by an actual need.
Revisit if `disco sandbox exec logs` against a running exec becomes a
workflow people rely on for near-real-time visibility.

## Consequences

**Consequence: an unclean process kill can still lose up to one bucket's
worth of output.** A graceful stop (the exec-shim catches `SIGTERM`) runs the
same drain-and-flush path as a normal exit. A hard kill (OOM, `SIGKILL`, a
forced container removal) can lose whatever was buffered since the last
flush — bounded to at most ~256KB or ~15 seconds, not unbounded, but not
zero. This is a real improvement over the previous state (100% loss of
everything on any restart, since tmpfs never survives one), not a regression.

**Consequence: taking a consistent backup of this database needs care.** A
straight `cp` of a live WAL-mode sqlite file can capture an inconsistent
snapshot (the `-wal`/`-shm` files matter). Backing this up safely requires
`VACUUM INTO` or an equivalent checkpoint-then-copy step. Not implemented as
part of this change — noted here as a follow-up for whoever wires up
sandbox-agent database backups.

**Consequence: retention is a fixed 14-day constant, not
sandbox-configurable.** Unlike `ResourceRetentionCount` (which flows from
`sandboxconfig.AgentRuntime` down from the control plane), log chunk
retention is not threaded through that layer. Nothing today needs it to
vary per sandbox; revisit if that changes.

## References

- `sandbox-agent/DESIGN.md` — current state of `execs`/`store` after this
  change.
- `docs/adr/0019` — the most recent precedent for this repo's ADR style.
