# gormdb

`gormdb` opens GORM connection pools for the database backends used by Discobox.

The public API is intentionally a single generic opener:

```go
pools, err := gormdb.Open(gormdb.Config{DSN: "sqlite3://app.db"})
```

or:

```go
pools, err := gormdb.Open(gormdb.Config{
    DSN:     "postgres://user:pass@localhost/app?sslmode=disable",
    ReadDSN: "postgres://user:pass@localhost/app_replica?sslmode=disable",
})
```

`Open` detects the driver from `DSN` unless `Config.Driver` is set explicitly.

## SQLite

SQLite uses the same split-pool pattern as Discobot:

- write pool: WAL, `busy_timeout(5000)`, `foreign_keys(1)`,
  `synchronous(NORMAL)`, `_txlock=immediate`, one open connection
- read pool: WAL, `busy_timeout(5000)`, `foreign_keys(1)`,
  `synchronous(NORMAL)`, `mode=ro`, `query_only(1)`, multiple read connections
- in-memory SQLite reuses the write pool for reads

Accepted SQLite DSNs:

- `app.db`
- `app.sqlite`
- `file:app.db`
- `sqlite://app.db`
- `sqlite3://app.db`
- `:memory:`

## Postgres

Postgres uses one pool for reads and writes by default:

- max open connections: 25
- max idle connections: 5

If `Config.ReadDSN` is set, reads use a separate pool opened from `ReadDSN`.

## Turso

Local Turso uses `turso.tech/database/tursogo`, which is a no-CGO
`database/sql` driver. Use a `turso:` or `turso://` DSN to select Turso from the
same DSN setting used for SQLite and Postgres:

```go
pools, err := gormdb.Open(gormdb.Config{
    DSN: "turso://app.db",
})
```

Turso Cloud sync also uses `tursogo`: reads and writes go to a local Turso
database file, and callers explicitly push/pull with `Pools.TursoSync`. Keep
the remote URL and auth token in separate config so they can come from
environment variables or another secret source:

```go
pools, err := gormdb.Open(gormdb.Config{
    DSN:              "turso://app.db",
    TursoDatabaseURL: os.Getenv("TURSO_DATABASE_URL"),
    TursoAuthToken:   os.Getenv("TURSO_AUTH_TOKEN"),
})

err = pools.TursoSync.Push(ctx)
changed, err := pools.TursoSync.Pull(ctx)
```

The GORM integration reuses the SQLite dialector over Turso's `database/sql`
drivers. Local Turso is close to the SQLite split-pool pattern, but its driver
does not accept `file:` URIs or enforce read-only mode through SQLite DSN
parameters. `gormdb` therefore uses raw paths for local Turso and configures the
local read pool with one connection plus `PRAGMA query_only = 1`.
