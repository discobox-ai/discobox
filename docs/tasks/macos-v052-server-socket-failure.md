# macOS v0.5.2 server socket failure investigation

## Objective and incident evidence

Reproduce or narrow down a server exit on macOS, then fix and verify the cause
if established. This is an investigation handoff, not an accepted diagnosis.

A user running Discobox v0.5.2 reported macOS **26.6.2** (as supplied, not
independently verified). They did not think the machine had slept. Connections
stopped working and later worked again. No further logs or information are
available; proceed without requiring anything more from this user.

The server log contained two pool-sync warnings, at 2026-09-09 23:45:12 and
2026-09-10 06:38:05, for pool `pool_bzrtb3x2b6f5fz10`:

```text
pool-sync failed ... default (code 401): ... unexpected Content-Type: application/json
```

There were successful image preload messages at 04:35:40 and 10:35:41 for the
v0.5.2 Claude Code, Codex, shell, and sandbox-agent images. The final line had
no timestamp:

```text
server failed: accept unix /var/folders/mg/c9v3lx6170x3z3tps568mv4c0000gn/T/discobox-501/discobox/server.sock: bad file descriptor
```

Do not infer the crash time from the last preload, or treat preloads as failures.
The `server failed` path in `server/internal/server/server.go:serveAll` shuts
down serving and returns the listener error. Pool-sync warnings in
`server/providers/poolruntime/provider.go:ReconcilePool` are best-effort and do
not themselves terminate the server. `endpoint/autolaunch.go:EnsureRunning`
can launch a replacement server on a later CLI invocation; that is a possible
explanation for recovery, not something the incident logs prove.

## Separate auth reporting fix

This change fixes `pool-agent/server/auth.go:reject` to emit
`application/problem+json`, matching `pool-agent/api/openapi/pool.yaml` and the
generated client. Previously it emitted `application/json`, hiding the response
behind a decoder error. The regression in
`pool-agent/server/auth_error_test.go:TestRefusalDecodesAsTheDeclaredErrorShape`
calls PoolSync through the generated client and real router, and asserts typed
401 errors with `missing_token` or `invalid_token` details. It reproduced the
reported Content-Type failure before the fix.

This does not fix an invalid token or the Unix listener failure. For a new 401,
collect the pool-agent's `rejected a control-plane request` log and its `error`
field, which carries the parse failure; the response's `invalid_token` detail
alone cannot distinguish expiry, clock skew, and a signature failure. Never
record bearer tokens or private keys. Guest RTC synchronization and token skew
allowances already existed in v0.5.2; sleep remains unproven.

## Descriptor ownership hypothesis

Both v0.5.2 and the checkout inspected for this handoff pin
`github.com/Code-Hex/vz/v3 v3.7.1` in `server/go.mod`. Verify the pin in the
checkout you actually test.

Inspect these dependency functions in the Go module cache:

- `socket.go:newVirtioSocketConnection`: wraps the framework descriptor in
  `os.NewFile`, calls `net.FileConn` (which duplicates it), and defers closing
  the original file.
- `virtualization_11.m:convertVZVirtioSocketConnection2Flat`: returns the
  framework's original `fileDescriptor` without duplicating it.
- The outgoing completion and incoming listener callbacks that supply the
  Objective-C connection and govern its lifetime.

The concern is that closing a descriptor owned by the framework may allow a
later framework close to hit a reused descriptor. Audit the framework ownership
contract and object lifetime before choosing a fix. Also establish how this
could affect the **already-open Unix listening socket**, rather than merely a
new VSOCK connection. A suspected double-close is not evidence that this exact
listener was its victim.

Discobox calls these paths from
`server/providers/vz/internal/vzvm/vm_darwin.go:Connect` and `Listen`; the driver
accepts guest connections and hands them to `carrierhub`. Useful external entry
points are [the Apple connection documentation](https://developer.apple.com/documentation/virtualization/vzvirtiosocketconnection)
and [the pinned binding](https://github.com/Code-Hex/vz/blob/v3.7.1/socket.go).
Do not assume a newer dependency version fixes it without inspecting the diff.

## macOS setup

1. Read root and applicable `server`, `server/providers`, and
   `server/providers/vz` DESIGN/REVIEW files. Work on the checked-out branch.
2. Record `sw_vers`, architecture, commit, Go version, dependency pin, actual
   guest image digest, and pool-agent image version. Prefer the reported OS
   version if available; clearly identify any different version used.
3. Use a dedicated test data/config directory, pool, and short Unix socket
   path. Inspect the current server/CLI help and configuration for how to set
   these. Do not reclaim a personal server's endpoint or modify its database.
   Capture timestamped server stdout/stderr from process start and the PID.
4. Enter `nix develop`; use `go tool task build:server` for the native server.
   This builds and codesigns with the Virtualization entitlement. `go run` or
   `CGO_ENABLED=0` does not exercise this backend correctly. If adding a native
   Go integration test that boots a VM, compile and sign its test executable
   through a Taskfile target before running it.
5. Confirm the test boots an actual vz guest. Existing fake-driver tests and
   Linux checks cannot validate Virtualization.framework descriptor ownership.
   Ensure the pool-agent running in the guest contains the auth reporting fix;
   rebuilding only the host server does not update an agent container image.

## Experiments

Add a repeatable opt-in macOS integration/stress target to `Taskfile.yml`, with
bounded duration and failure artifacts. Run an unmodified baseline before any
VSOCK patch, and repeat the same workload after a candidate fix.

1. **Control:** serve HTTP on a Unix listener and repeatedly probe it without
   starting a VM. Record failures and descriptor counts.
2. **VSOCK churn:** boot a guest and exercise both host-to-guest connections
   (Docker on port 3004 and pool API on 3002) and guest-to-host connections
   (control plane on 3001). Open, exchange data, and close in serial and then
   concurrently. Include abrupt peer disconnects and cancellation. Disable
   connection reuse where necessary so this creates connections instead of
   repeatedly using one HTTP keepalive connection.
3. **Sentinel listener:** keep the same Unix listener open throughout churn.
   Probe it on independent new connections, recording every Accept/probe error,
   process PID, iteration, and elapsed time. Start with 10,000 connection cycles
   and a 30-minute time limit; make these configurable. Run with the listener
   allocated before VM startup, as in the real server. A supplementary case
   that allocates listeners during churn may expose descriptor reuse, but must
   not be presented as reproducing the incident's allocation order.
4. **Lifecycle:** repeat dedicated test-pool stop/start cycles under traffic,
   observing teardown and pending accepts. Bound shutdown waits so a hang
   produces diagnostic stacks instead of wedging the test. Use existing pool
   lifecycle operations rather than deleting persistent state to recover.
5. **Full application:** run the native server, create disposable sandboxes,
   execute/attach/detach, and poll its Unix endpoint while the workload runs.
   Keep the observer from autolaunching a server: a restart would mask failure.
   Separately test CLI autolaunch after an intentional orderly server stop,
   recording old/new PIDs to validate the proposed recovery mechanism.
6. **Optional sleep/wake:** only after awake runs, repeat with an intentional
   sleep/wake cycle and collect host/guest clock offsets and auth logs. Label
   this as a separate experiment, not an established incident precondition.

If a descriptor failure occurs, capture native and Go stacks and descriptor
allocation/duplication/close history, including ownership and thread identity.
Use a debugger or suitable OS tracing if available; macOS tracing permissions
may limit this. Otherwise instrument a local dependency checkout used through
a temporary module replacement. Do not edit the shared Go module cache, print
credentials, or commit an absolute local replacement path. Track descriptor
lifetimes, not just integer values, because the kernel reuses numbers. A close
stack hitting the sentinel listener's live descriptor would be much stronger
evidence than another unrelated VSOCK EBADF.

## Completion criteria and return report

- Report each workload's OS/build/image versions, duration, connection and
  lifecycle counts, descriptor trend, errors, and whether the PID changed.
- Preserve the exact reproduction command/Taskfile target and relevant logs.
- For a fix, demonstrate the failing baseline and passing candidate on the same
  workload, plus successful traffic in both directions and bounded resource
  use after teardown. Validate descriptor ownership, not just disappearance of
  one error. Do not suppress EBADF or retry on a dead listener as the fix.
- Run relevant tests and `go tool task check-hooks` before handing back code;
  run build-related CI checks where applicable per AGENTS.md.
- If no failure reproduces, say **not reproduced** with the tested coverage.
  Do not mark the incident fixed or the hypothesis confirmed. Report what
  evidence remains missing and the next useful experiment.
