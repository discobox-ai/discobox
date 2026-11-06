# Server Review Notes

- The `github.com/Code-Hex/vz/v3` replace in `go.mod` is at or past the one
  disco-vm's own `go.mod` names, never behind it. Check it after any rebase
  that touched `go.mod`. Why: disco-vm's vz driver uses fork-only API
  (`WithMacGuestProvisioning`), so an older pin only fails CI's `darwin` job,
  which Linux cannot typecheck locally. A rebase taking main's side of
  `go.mod` already put the older pin back once.
- Retiring a `server.yaml` key keeps a `Retired*` field for it in
  `internal/config.Config`, with its `yaml` tag and no `env` tag, beside its
  `retiredKeys` entry, and a test that loads a file setting it. Why:
  `configfile.Decode` refuses unknown keys (`KnownFields(true)`), so deleting the
  field makes a file that still sets the key stop the server before the
  retired-key warning can run — `overlayDir` was retired that way once.
- Removing a field a response schema in `api/openapi/server.yaml` lists as
  `required` breaks every client generated before it on every route that
  returns the schema, not only the route the field was about: ogen decodes a
  missing required field as an error. Say so where the removal is decided (an
  ADR's Consequences, a `!` commit). `configured` on the harness config was
  removed with configure, and the first draft claimed only configure's own
  routes broke.
- A test that drives a streaming handler without a server (a recorder over
  `io.Pipe`, as under `testing/synctest`) gives back what the server did: the
  recorder hands bytes to the reader on `Flush`, and when the handler returns
  it hands over what is still pending and closes the pipe's writer, and the
  request keeps a deadline. Why: without those, a handler that stops flushing
  still passes, a response written and never flushed (an error before the
  stream starts) reads as empty, and one that ends early or never reaches the
  expected line deadlocks or spins on the fake clock instead of printing what
  it read (`TestSandboxEventsStreamsServerSentEvents` lost all of it when it
  moved into a bubble).

- A shutdown request and an incumbent health probe have independent chances to
  answer. Observe valid starting/503 health before waiting on a slow shutdown,
  and bound discovery across all endpoints. A shared deadline once let a hung
  shutdown hide a reachable starting server and misreport it as unreachable.
- Singleton refusal reaches the launching CLI through its per-launch error
  record, including systemd launches that have no child process handle. A
  useful log tail alone still left those launches reporting `never answered`.

- Validate a WebSocket handshake and Origin before executing query-supplied
  commands. Rejecting an upgrade after opening the provider is too late: an
  ordinary or cross-origin request can already have run a command on the host.
- Pool console output errors must fail commands and close both output readers;
  treating a broken driver attach as EOF can hang waiting for a process whose
  output was truncated, or report its later successful status.
