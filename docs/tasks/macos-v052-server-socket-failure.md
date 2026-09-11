# macOS v0.5.2 server socket failure investigation

## Result (2026-09-10)

**Listener EBADF reproduced and fixed.** A sleep/wake trigger was not tested.

Environment: macOS 26.6.2 (25G83, the reported version), arm64, go1.27.1,
`Code-Hex/vz/v3 v3.7.1`, guest `discobox-vm@sha256:af1d6ee4…` (the v0.6.0
pin), 4 vCPU / 4 GiB, throwaway disks. The workload is 8 workers each churning
VSOCK host→guest (Docker relay, port 3004; every fourth connection dropped
with the request in flight), plus 8 workers probing a Unix HTTP listener
opened before the VM, one new connection per probe.

Reproduce: `go tool task test:vz-stress GUEST=<guest image dir>`, or `NO_VM=1`
for the control. `CYCLES`, `DURATION`, `WORKERS` bound it; `PAUSE=90s` pauses
the guest through the framework partway through and reports its clock lag.
Artifacts go to `build/vz-stress`.

| Run | Binding | Serve outcome | VSOCK cycles | Probes | FDs before / after / teardown |
| --- | --- | --- | --- | --- | --- |
| control ×1 | no VM | ok | 0 | 20,001, 0 errors | 6 / 6 / 6 |
| baseline 1 | v3.7.1 | `accept unix …: bad file descriptor` at 23 ms | 144 | 355, 4 errors | 7 / 6 / 6 |
| baseline 2 | v3.7.1 | `accept unix …: bad file descriptor` at 7 ms | 45 | 104 | 7 / 6 / 6 |
| baseline 3 | v3.7.1 | `accept unix …: setnonblock: bad file descriptor` at 7 ms | 42 | 111, 3 errors | 7 / 6 / 6 |
| baseline 4 | v3.7.1 | `accept unix …: setnonblock: bad file descriptor` at 34 ms | 222 | 479, 7 errors | 7 / 6 / 6 |
| candidate ×4 | patched (local) | ok, ~1.4 s each | 10,000 each | 20,765–22,258, 0 errors | 7 / 7 / 7 |
| fork ×2 | `discobox-ai/vz` `cfc8ce37` | ok, ~1.4 s each | 10,000 each | 20,715–20,875, 0 errors | 7 / 7 / 7 |

Baseline side errors include probe `read: bad file descriptor` and
`dial unix …: bad file descriptor`, and a VSOCK `fcntl: bad file descriptor`
(the framework's own fd already gone when vz dup'd it). Each run is one
process; nothing restarted.

Mechanism:

- Apple's header: the `VZVirtioSocketConnection` fd "is owned by the
  VZVirtioSocketConnection. It is automatically closed when the object is
  destroyed." vz's `newVirtioSocketConnection` dups it through `net.FileConn`,
  then closes the original. The framework's later close hits whatever reused
  the number. Upstream main (26 commits past v3.7.1) is unchanged here.
- The listener is never the victim. The error form identifies the fd that is.
  In Go (same code in 1.26.1, v0.5.2's toolchain, and 1.27.1), a dead
  listener prints `accept unix P: accept: bad file descriptor`. If the
  just-accepted fd vanishes before `SetNonblock`, it prints
  `…: setnonblock: bad file descriptor`. The incident's bare
  `accept unix P: bad file descriptor` comes from `netFD.init`, meaning
  kqueue registration of the **just-accepted** fd failed: another thread
  closed it between `accept(2)` and registration. `http.Server.Serve` treats
  that as fatal, so `serveAll` exits.

Fix: `discobox-ai/vz`, branch `discobox`, commit `cfc8ce37` on `v3.7.1`. It
dups the fd inside the completion handler or listener delegate, while the
object is alive, and leaves the original to the framework. `server/go.mod`
replaces the upstream module with it. No upstream PR has been opened yet, by
decision.

### Fixed server, full application

This checkout's `discobox-server` (with the fork) was built with the release
image flags pinned to `v0.6.0`. It ran with its own data, config, cache and
state directories and no env file, listening only on `/tmp/dbxfix/s.sock`. It
had its own vz pool, guest `discobox-vm@sha256:af1d6ee4…`, pool agent
`discobox-pool-agent:v0.6.0` (which predates the auth reporting fix), and one
shell-harness box. Every CLI call passed `--auto-start-server=false`. The
driver scripts were ad hoc and are not committed.

| Phase | Traffic | Result |
| --- | --- | --- |
| Awake | 5 min, 4 workers looping `admin exec create` (64 KiB of output each) | 3,133 execs, 0 failures |
| Freeze | 12 min of the same traffic; 60 s in, the server and its VM process SIGSTOPped together for 400 s | 1,660 execs, 0 failures |

- **Server:** PID 79160 throughout. `/healthz` was probed on a new connection
  every 0.5 s: 2,402 × 200. The only non-200s were the 109 probes during the
  freeze, and the first probe after resume succeeded.
- **Log:** no `server failed`, `bad file descriptor`, pool-sync failure or
  401. Server fds went 33 → 58 at peak → 34 at the end.
- **Execs during the freeze:** they waited in the socket backlog and completed
  after resume. The scripts' client timeout never fired, because the Go CLI
  ignores SIGALRM.
- **The clock:** SIGSTOP is not sleep. The first host/guest skew sample after
  resume came 91 s later, after the guest's 30 s clock step, so this run says
  nothing about clock lag or the post-wake 401 path.

### Framework pause

Command: `go tool task test:vz-stress GUEST=… WORKERS=2 CYCLES=400000
DURATION=5m PAUSE=90s`, run with the fork. Ten seconds into the churn, the
guest was paused through Virtualization.framework, held for 90 s, then resumed.

- **The guest clock froze with it.** The guest was 89.5 s behind the host
  from resume until its RTC step at about +18 s, then 0.4 s off.
- **Unix listener:** 4,233,597 probes, 0 errors. `Serve` never failed, and
  fds stayed 7 / 7 / 7.
- **VSOCK:** 400,000 cycles, 16,433 errors. The 500 errors recorded are the
  framework refusing connections while the VM was `pausing` or `paused`
  ("must be running to connect"). The total is consistent with two workers
  retrying every 10 ms for 90 s. Churn resumed after the pause.

Not covered:

- **Real sleep/wake.** The process freeze and the framework pause are stand-ins;
  what macOS itself does to the host during sleep is untested. The race needs
  no sleep: a VSOCK connection only has to be created in the instant between a
  Unix accept and its registration. The burst of concurrent VSOCK and CLI
  connections on wake is a plausible link, but it was not measured.
- **Guest→host churn (port 3001).** It ran only as the live pool agent's own
  control-plane traffic in the full-app run. That path calls the same fixed
  function, but it was not churned deliberately.
- **Pool lifecycle** (stop/start under traffic) and native close stacks. The
  evidence is the error form plus 4 failing unpatched runs vs 6 passing
  patched runs on the same workload.
- **Post-wake 401s.** A paused guest's clock stays behind until its next
  30-second RTC step. So a sleep longer than the five-minute token lifetime
  would put the guest past that lifetime for up to 30 s after wake, which fits
  transient pool-sync 401s. This was not reproduced, and the pool agent image
  used lacked the auth reporting fix below.

## Objective and incident evidence

Reproduce or narrow down a server exit on macOS, then fix and verify the cause
if established. This is an investigation handoff, not an accepted diagnosis.

A user running Discobox v0.5.2 reported macOS **26.6.2** (as supplied, not
independently verified). They initially did not think the machine had slept,
but subsequently confirmed that the failure seems to occur after system sleep.
Treat sleep/wake as the priority reproduction path, with a reported correlation
rather than a proven mechanism. Connections stopped working and later worked
again. No additional logs are available; proceed without requiring anything
more from this user.

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
allowances already existed in v0.5.2. The reported sleep correlation makes
post-wake clock behavior worth measuring, but does not prove the cause of a 401
or explain the listener EBADF.

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

1. **Sleep/wake (priority):** run the native server with a real vz guest and
   disposable sandbox. Establish healthy Unix endpoint and pool API probes,
   then suspend and resume the Mac. Confirm actual system sleep/wake from OS
   power events, not just display sleep. Repeat with idle and active VSOCK
   traffic, and with short and longer sleeps. Record sleep duration, host/guest
   UTC offsets immediately after wake and through at least several 30-second
   RTC-sync intervals, auth rejection causes, VM state, endpoint probe errors,
   and server PID. Keep probes from autolaunching a replacement server. Observe
   whether 401s recover as the guest clock catches up and whether the Unix
   listener fails independently. If EBADF occurs, capture the descriptor
   evidence described below. Compare with an awake run of the same workload.
2. **Control:** serve HTTP on a Unix listener and repeatedly probe it without
   starting a VM. Record failures and descriptor counts.
3. **VSOCK churn:** boot a guest and exercise both host-to-guest connections
   (Docker on port 3004 and pool API on 3002) and guest-to-host connections
   (control plane on 3001). Open, exchange data, and close in serial and then
   concurrently. Include abrupt peer disconnects and cancellation. Disable
   connection reuse where necessary so this creates connections instead of
   repeatedly using one HTTP keepalive connection.
4. **Sentinel listener:** keep the same Unix listener open throughout churn.
   Probe it on independent new connections, recording every Accept/probe error,
   process PID, iteration, and elapsed time. Start with 10,000 connection cycles
   and a 30-minute time limit; make these configurable. Run with the listener
   allocated before VM startup, as in the real server. A supplementary case
   that allocates listeners during churn may expose descriptor reuse, but must
   not be presented as reproducing the incident's allocation order.
5. **Lifecycle:** repeat dedicated test-pool stop/start cycles under traffic,
   observing teardown and pending accepts. Bound shutdown waits so a hang
   produces diagnostic stacks instead of wedging the test. Use existing pool
   lifecycle operations rather than deleting persistent state to recover.
6. **Full application:** run the native server, create disposable sandboxes,
   execute/attach/detach, and poll its Unix endpoint while the workload runs.
   Keep the observer from autolaunching a server: a restart would mask failure.
   Separately test CLI autolaunch after an intentional orderly server stop,
   recording old/new PIDs to validate the proposed recovery mechanism.

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
