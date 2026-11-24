# 26-11-24-664 — The iroh endpoint key is kept sealed in the database, and the iroh endpoint binds once it is read

- **Status**: Proposed
- **Date**: 2026-11-24
- **Issue**: [#1551](https://github.com/discobox-ai/discobox/issues/1551)
- **Would supersede**:
  [0117](0117-every-server-has-a-peer-id.md) §1's `<data dir>/iroh_endpoint_key`
  and its Consequence that the key "is backed up and moved with the data
  directory"; and the server-key cell of
  [0052](0052-iroh-is-an-optional-endpoint-scheme.md) §5's table, with its "the
  identity comes from the key file". What 0117 says the identity is for, and
  that every server loads it on every start, stands.
- **Would narrow**:
  [0095](0095-an-enrolled-iroh-id-is-a-managed-resource.md) §4's "`configureIroh`
  runs before `database.New`": the iroh endpoint now binds after the database
  and the keyring are open, and still before the store exists, so Admission
  still learns the store later.

## Context

The server's iroh identity is an ed25519 key in a file,
`<data dir>/iroh_endpoint_key`, PKCS#8 PEM.
`irohd.LoadOrCreateEndpointKey` (`server/internal/irohd/endpointkey.go`)
generates it on the first start with `privatefs.WriteExclusive` (private from
creation, first writer wins) and restricts it again on every load. Its public
half is the server's peer ID (ADR 0117), served by `GET /server/peer` and
logged at every start. It is never regenerated implicitly: a new key is a new
address, which breaks every client's `iroh://`/`discobox://` endpoint and every
registration recorded by peer ID.

Its only caller, `configureIroh` (`server/internal/server/iroh.go`), runs before
`database.New` (`server/internal/server/server.go`). The server binds every
listen endpoint before its slow startup work (staging release images, opening
and migrating the database, connecting the encryption provider, building the
services), so that a client sees "opening the database" rather than a refused
connection. The iroh endpoint needs the key to bind, so the key is read first.

Everything else the server keeps secret is in the database, sealed by the
keyring in its scope (ADR 26-11-02-902): each project's values in that
project's scope, and the key the server signs pool-agent requests with in the
`server` scope, in the `server_state` row `worker_agent_request_issuer`
(`server/internal/auth/poolagent/auth.go`). With no `encryption.provider` they
are stored unsealed; #40 is to turn sealing on by default, with the encryption
key in the OS keychain.

#40's owner decision (2026-11-21) kept the server's key files as files for the
time being, and opened #1550 (SSH host key) and this issue to decide each. The
triage leaned towards keeping the iroh key a file. The owner decided otherwise
(2026-11-24, on this issue): the key is stored in the database, sealed, and the
OS keychain holds the encryption key alone, not the other things the server
keeps secret. On #1550 the owner also said the supplementary `authorized_ids`
stays on disk.

What a leaked key costs: whoever holds it can answer as this server to a client
that dials its peer ID. Nothing stored is exposed by it. It is less sensitive
than the encryption key, but it is a credential, and a copy of the data
directory is today a copy of it, unsealed, whatever the server's encryption
setting.

## Decision

### 1. The key is a `server_state` row, sealed in the server scope

The key is kept in `server_state` under the key `iroh_endpoint_key`, beside the
pool-agent issuer key and kept the same way:

- Its private half is sealed with `encryption.Field{Scope: ServerScope,
  Purpose: "iroh_endpoint_key.private_key", ResourceID: "iroh_endpoint_key"}`,
  so the sealed bytes open nowhere else. The value also holds the public half
  (the peer ID), unsealed. How the value is encoded is the implementation's.
- With no `encryption.provider`, the private half is stored unsealed, exactly
  as every other sealed field is today. When #40 makes sealing the default,
  this key is sealed with no further change.
- A key stored unsealed, or sealed by an earlier release, is sealed again in
  the current format at startup once a provider is configured, as ADR
  26-11-02-902 §4 does for every sealed value. A provider change is a rewrap of
  the `server` scope (§5 there), which touches no row.
- A sealed key with no provider to open it fails startup, naming
  `encryption.provider`. The server never generates a key in its place: that
  would change its identity without anybody asking.

### 2. Created once, first writer wins, never regenerated

With no row (and no file to import, §4), the server generates a key and
creates the row. If the create conflicts with a row another process wrote
first, it reads that row and uses its key, which is the property
`privatefs.WriteExclusive` gives the file today. Nothing rotates the key. A
rotation command stays out of scope, as it is today.

### 3. The iroh endpoint binds once the key is read; every other listener binds as today

Startup becomes:

1. Every listen endpoint that is not iroh binds before the database opens, as
   today, and answers with the startup handler.
2. The database opens and migrates, and the keyring connects.
3. The key is read or created (§§1–2, §4). The peer ID is logged here, on every
   start, as ADR 0117 §1 requires, still before the iroh endpoint binds, so the
   one client ADR 0052 §6 describes can still find it in the log.
4. If a listen endpoint names `iroh://` or `discobox://`, iroh's logging, the
   endpoint and Admission are set up and the endpoint binds. From then on it is
   served by the same startup handler as the other listeners, reporting the
   phases still to come.
5. `NewApp` returns and Admission is handed the store, as ADR 0095 §4 does now.
   A peer that connected between steps 4 and 5 waits for it, as one that
   connected before the database opened waits today.

A failure to bind the iroh endpoint still fails startup. It now does so after
the database has opened, rather than before.

### 4. An existing key file is imported once, then removed

On the first start of a server with this change:

| Row | `<data dir>/iroh_endpoint_key` | What the server does |
| --- | --- | --- |
| absent | absent | Generates a key (§2). |
| absent | present | Stores the file's key (§§1–2), then deletes the file. The peer ID does not change. |
| present | absent | Uses the row. |
| present | present, same key | Deletes the file. This is a start interrupted between the row's create and the file's deletion. |
| present | present, another key | Refuses to start, naming the file and both peer IDs. Each is some server's identity, and choosing one silently could hand this server's address to the wrong one. |

A file that is not a parseable ed25519 key fails startup, as it does today. The
file is deleted only after its row is committed, so a crash in between leaves
the same key in both places and the next start completes the import. After
import the server never reads or writes the file again.

### 5. What stays in the data directory

`authorized_ids`, the operator-edited admission file, stays a file, checked
before the database exactly as ADR 0095 §3 says. So does the record of the
sockets the iroh endpoint last bound (`irohd.RememberSockets`), which is not a
secret. Only the key moves.

### 6. The identity is backed up with the database

Losing the database, not the data directory, now changes the server's peer ID.
A server on SQLite in its default place keeps both in the data directory, so a
copy of the directory still carries the identity. A server on PostgreSQL carries
it in the database. With a provider configured, a backup restores the identity
only with the provider's key as well, the same as every sealed secret.

## Alternatives rejected

- **Keep it a file.** This was the triage's recommendation: `privatefs` already
  makes it private on every platform, a file works headless and in containers,
  and ADR 0117's rule that copying the data directory copies the identity
  would hold. Rejected by the owner: no secret needs to be on disk outside the
  store that seals secrets, and a file stays unsealed whatever the server's
  encryption setting.
- **The OS keychain.** Rejected by the owner: the keychain holds only the
  encryption key (#40). Putting more there would also take on #40's questions
  for this key (naming the entry when two servers share an account, a headless
  fallback), and a copy of the data directory or database would no longer
  carry the identity.
- **Open the database before binding any listener.** This would keep a single
  bind step. Rejected: every client, including the CLI that autolaunched the
  server over its unix socket, would see a refused connection through release
  staging, migration and provider connection, which is exactly what binding
  first prevents.
- **Bind the iroh endpoint after `NewApp`, so Admission is built with its
  store.** This would remove ADR 0095 §4's wait. Rejected: iroh clients would
  get no answer through service start and the built-in harness check as well,
  and the wait already exists and is bounded by the listener's context.
- **Keep the file as a cache read before the database, so the iroh endpoint
  binds first as today.** Rejected: an unsealed copy on disk is what this
  decision removes. It would also leave two copies of one identity that can
  drift.
- **Keep the file for a release after importing it, so a downgrade finds it.**
  Rejected for the same reason: the unsealed copy would outlive the change.
- **A table of its own for the server's keys.** Rejected: `server_state` already
  holds the server's pool-agent issuer key, sealed in the `server` scope, and a
  table for one more row adds a migration and nothing else.

## Consequences

- A fresh data directory has no `iroh_endpoint_key` file. An upgraded one
  loses its file on the first start, and keeps its peer ID.
- A downgrade to a release before this one, after the import, finds no file and
  generates a new key: the server answers at a new peer ID, and clients
  registered by the old one must register it again.
- Over iroh, a starting server answers nothing until its database has opened,
  migrated and connected to its provider, including while release images are
  staged. Over every other transport, startup looks as it does today.
- A broken database, or an unreachable encryption provider, now keeps the iroh
  endpoint from binding. Both already fail startup before the server serves
  anything, so no working path is lost.
- Servers that share one database share one peer ID.
- `discobox-secret-dump` and `secret reveal` are unchanged: the key is not a
  project's secret and neither lists it.
- `server/DESIGN.md` ("The Server's Own Peer ID"), `server/internal/sshd/DESIGN.md`
  ("Persistent host key", which says the iroh key is kept the same way as the
  host key) and the sandboxes design note on a lost `iroh_endpoint_key` are
  updated with the implementation.
- Whether the SSH host key is kept the same way is #1550's decision, not this
  one's. Its key is already read after the database opens, so it would need
  §§1, 2 and 4 here and nothing from §3.
