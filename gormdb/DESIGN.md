# gormdb Design

## Package Role

`gormdb` is the shared database opener for Discobox components that use GORM.
It owns backend detection, GORM dialector selection, and read/write `*gorm.DB`
pool construction. Callers provide `Config`; callers own schema migration,
transactions, and repository-level query behavior.

`Open` returns a `Pools` value with:

- `Write`: the pool used for writes and migrations.
- `Read`: the pool used for read-only query paths when a separate read pool is
  available.
- `Driver`: the selected backend.
- `TursoSync`: optional explicit sync controls for Turso Cloud synced local
  databases.

## Backend Selection

`Config.Driver` is authoritative when set. Otherwise `DetectDriver` infers the
backend from `Config.DSN`:

- `postgres://` and `postgresql://` use Postgres.
- `turso:` and `turso://` use Turso.
- SQLite-style paths and prefixes use SQLite by default.

Raw Turso paths overlap with SQLite path syntax, so DSN-driven configuration
should use the `turso:` or `turso://` prefix when selecting Turso from a single
environment-provided DSN. Callers may still set `Driver: DriverTurso` explicitly
for raw local Turso paths.

## SQLite Pool Pattern

SQLite uses `github.com/glebarez/sqlite`, a pure-Go modernc-based GORM SQLite
dialector/driver stack.

File-backed SQLite databases use split pools:

- Write pool: one open connection, WAL, `busy_timeout(5000)`,
  `foreign_keys(1)`, `synchronous(NORMAL)`, `_txlock=immediate`, and `mode=rwc`.
- Read pool: multiple read connections, WAL, `busy_timeout(5000)`,
  `foreign_keys(1)`, `synchronous(NORMAL)`, `mode=ro`, and `query_only(1)`.

In-memory SQLite reuses the write pool as the read pool because separate
in-memory SQLite connections do not share state.

SQLite ignores `Config.ReadDSN`; its read pool is derived from `Config.DSN`.

## Postgres Pool Pattern

Postgres uses `gorm.io/driver/postgres`.

By default, Postgres uses the same pool for reads and writes with 25 max open
connections and 5 max idle connections. When `Config.ReadDSN` is set, `gormdb`
opens a separate read pool from that DSN with the same connection limits.

## Local Turso Pool Pattern

Local Turso uses `turso.tech/database/tursogo`, a no-CGO `database/sql` driver
for the new Turso Database engine. `gormdb` reuses the GORM SQLite dialector with
the Turso driver name.

Local Turso follows the SQLite split-pool shape where the driver supports it:

- Write pool: one open connection, SQLite-compatible pragmas, and raw path DSNs.
- Read pool: separate pool derived from the write DSN.

Important local Turso differences from SQLite:

- The local Turso driver does not accept `file:` URI paths, so `gormdb` uses raw
  paths for local Turso.
- The local Turso driver does not enforce read-only mode from SQLite DSN flags,
  so `gormdb` sets the read pool to one connection and applies
  `PRAGMA query_only = 1` on that connection.
- Local Turso ignores `Config.ReadDSN`; its read pool is derived from
  `Config.DSN`.

In-memory local Turso reuses the write pool as the read pool for the same reason
as in-memory SQLite.

## Turso Cloud Sync Pattern

Turso Cloud support uses the new Turso Database sync model, not the legacy libSQL
direct client. Reads and writes go to a local Turso database file opened with
`tursogo`; callers explicitly synchronize that local database with Turso Cloud.

Use a `turso:` / `turso://` DSN to select Turso from a single DSN setting. Set
`Config.TursoDatabaseURL` to open a synced local Turso database while keeping the
remote URL out of the DSN. `Config.TursoRemoteURL` is a compatibility alias for
`TursoDatabaseURL`. A `remote_url` query parameter is still accepted for callers
that need a fully DSN-driven configuration, but explicit config fields override
it. Keep `Config.TursoAuthToken` separate so callers can source it from an
environment variable or secret store without embedding it in a logged DSN.
`gormdb` exposes the sync lifecycle as `Pools.TursoSync`:

- `Push` sends local changes to Turso Cloud.
- `Pull` fetches remote changes into the local database.
- `Checkpoint` checkpoints the local WAL.
- `Stats` returns sync statistics.

By default, synced Turso uses the same GORM pool for reads and writes. When
`Config.ReadDSN` is set, `gormdb` opens a separate read pool against the same
synced local database. `ReadDSN` acts as an opt-in for separate read-pool
ownership; the synced database path still comes from `Config.DSN`.
