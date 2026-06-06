# gormdb

`gormdb` opens GORM connection pools for the database backends used by Disco2.

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
