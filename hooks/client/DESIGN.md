# Client Design

The client package owns communication with a session hook daemon over a Unix
domain socket. It is used by the CLI and any future in-process integrations that
need to control or inspect a running daemon.

## Responsibilities

- Compute or accept the session socket path.
- Connect to the daemon over Unix domain socket.
- Implement request timeouts and connection errors clearly.
- Provide typed methods for daemon API operations backed by the generated
  `hooks/api/gen` client for non-streaming endpoints.
- Decode daemon responses into public client DTOs.
- Avoid direct database writes.

## Protocol Shape

Prefer HTTP over Unix domain socket for debuggability and standard tooling. The
client can hide the transport behind typed methods such as:

- `Ping` / `PingInfo`
- `Status`
- `ListHooks`
- `ListRuns`
- `ListObservedChanges`
- `ListWorkspaceSnapshots`
- `ListQueue`
- `PauseAll`
- `ResumeAll`
- `PauseHook`
- `ResumeHook`
- `RunHook`
- `Output`
- `Shutdown`

If a lighter JSON-RPC protocol is chosen later, keep this package as the only
transport boundary so CLI commands do not depend on protocol details.

`RunHook` carries both `force` and optional `phase` in its request body. The
client only serializes the phase selected by the CLI or caller; service/manager
own validation and phase activation.

## Version Handshake

`PingInfo` returns daemon metadata including the session ID and numeric daemon
version. The CLI uses this metadata before normal commands: if the client build
version is newer than the daemon version, it requests daemon shutdown and starts
the current executable as the replacement daemon.

## DTO Ownership

Define socket API DTOs with daemon/client needs in mind. Do not reuse GORM models
or server API DTOs directly. DTOs should be stable enough for CLI compatibility
but can remain internal to the hooks module until there is an external API
consumer.

## Error Handling

Distinguish:

- daemon not running / socket missing
- socket exists but connection refused
- request timeout
- daemon returned validation error
- daemon returned hook execution failure metadata

The CLI startup path uses these distinctions to decide whether to spawn a daemon
or report a real runtime error.

## Non-Responsibilities

- Do not start daemons directly; startup orchestration belongs to the CLI/daemon
  bootstrap code.
- Do not parse hook files.
- Do not mutate SQLite directly.
