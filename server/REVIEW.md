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
- A proxy shared by routes with different contracts answers its own failures
  in the contract of the route it serves, never one shape for all: the
  sandbox-agent API decodes `{"error": …}`, while git and a box's ports show
  plain text. The first fix for a bare 502 gave the git and port routes, here
  and in the pool agent, the agent API's JSON and its "may be restarting".
- A retry that waits out a hop to a pool keys on what only the hop says: a
  connection that failed, or a 5xx not in the API's JSON error shape. A bare
  status cannot tell a front's 502 from the pool agent's or sandbox agent's
  own, which are answers to hand on at once; the first cut of the attach
  retry held those for 30 seconds, and retried a token that could not be
  minted, which is no connection's failure.
- A listener bound to loopback is still reachable from a sandbox, so one that
  serves anything secret needs its own credential as well. Why: Docker
  Desktop forwards `host.docker.internal` to the host's `127.0.0.1`, and the
  pool proxy refuses only destinations that resolve to loopback, so a Docker
  Desktop pool's sandbox reaches the host's loopback ports through it
  (`hostGatewayURL` relies on the same forwarding). The pprof listener's first
  draft trusted loopback alone, which handed a sandbox the server's command
  line, every goroutine's stack, and profiles and traces long enough to stall
  it.
- A provider factory or `Initialize` that reads a field of the instance row
  (`model.SandboxProviderInstance`) adds it to `sandbox.buildKey`, the
  provider cache's key. Why: the cache keeps a provider across any write that
  leaves the key unchanged, so a field read but not keyed is served stale
  after an update; the key left out `Name` and `Disabled` on purpose so a
  rename or a disable does not rebuild the provider and power its pools' VMs
  off (#1556).
