# 26-11-15-089 — A provider instance is checked on request, without making anything

- **Status**: Accepted (§3's check of a disabled instance on a provider built for the check alone superseded by [26-11-22-507](26-11-22-507-a-disabled-provider-takes-no-new-pools-and-its-existing-pools-keep-working.md): a disabled instance is checked on its cached provider like any other)
- **Date**: 2026-11-15

Accepted by the owner by merging #1207, 2026-11-15.
- **Amended**: 2026-11-15, §3, during implementation and before anything shipped: a second marker, `sandbox.ErrProviderNotReachable`, for a service that does not answer when a provider is built; and a disabled instance is checked on a provider built for the check alone.
- **Issue**: [#1200](https://github.com/discobox-ai/discobox/issues/1200)
- **Preserves**: [26-11-07-306](26-11-07-306-initial-pool-bootstrap-waits-on-work-evidence.md) §2's work evidence (nothing here renews a silence budget); [26-11-09-593](26-11-09-593-a-never-registered-pool-gets-a-new-bootstrap-attempt-after-one-ends.md) §2's expiry pass; [26-11-11-080](26-11-11-080-each-disco-vm-driver-is-a-provider-type-of-its-own-name-configured-by-its-own-fields.md) §§3, 8, every dial checked against the operator's endpoint policy; provider create checking only the config.

## Context

A live run on boxd (#1050) took three attempts. Its server was started under
`env -i`, with no proxy environment, so it had no route out. Nothing said so
until the pool failed, and then the pool said the wrong thing:

```
bootstrap : bootstrap stopped making progress
```

The real error was `boxd: exchange API key: Post "https://app.boxd.sh/...":
dial tcp …:443: connect: network is unreachable`. Only the reconcile log had it.

Checked against `main` at `9883798b8`:

- **Nothing can check a provider instance.** `api/openapi/server.yaml`
  `/projects/{projectId}/providers{,/{providerId}}` is CRUD alone, and
  `SandboxProviderInstance` has no health. Create validates the config and
  calls nothing (`sandbox.ProviderManager.ValidateProviderConfig`,
  `server/internal/sandbox/manager.go`). So `scripts/live-run.sh`'s preflight
  (`admin provider list`) proves the server answers. It does not prove the
  server reaches the provider.
- **The check exists, inside the pool's create.** `discovm`'s
  `poolMachine.create` (`server/providers/discovm/poolmachine.go`) calls the
  disco-vm driver's `Driver.Check` before it makes a pool's machine. boxd's
  `Check` asks `Whoami` and, with a region, lists the region's machines. A
  service that does not answer is marked `sandbox.ErrPoolNotReachable`
  (`notReachable`); a refusal is not.
- **`Provider.Status()` is not this.** It takes no context, describes a
  provider *type* in the catalog (`/providers/catalog`), and
  `poolruntime.Provider.Status` answers `ready` always.
- **The wait loses the cause.** The pool reconciler requeues on
  `ErrPoolNotReachable` and records nothing
  (`server/internal/resources/pools/reconciler.go`, `reconcileActive`). When
  the attempt's silence budget runs out, the expiry pass writes
  `PoolBootstrap.Expired`'s reason alone (`server/internal/model/pool_bootstrap.go`):
  "bootstrap stopped making progress".
- `poolruntime.Provider` is the one `sandbox.Provider`. Beneath it,
  `poolruntime.RuntimeProvider` has two implementations, `dockerworker.Engine`
  and `discovm.Runtime`, and the engine has six `dockerworker.Driver`s
  (`docker`, `digitalocean`, `exec`, `libkrun`, `vz`, `wslc`).

The issue's "Done when" is that a run on a server with no egress stops at
preflight, naming the unreachable provider, before any pool is made. That
needs a way to ask the server whether it reaches a provider instance.

## Decision

### 1. A route checks one provider instance, on request

`POST /projects/{projectId}/providers/{providerId}/check` asks the instance's
provider, as the server resolves it, whether it can reach what the instance
needs, and makes nothing. It is authorized as the project's other provider
routes are.

It is `POST` because it is not a read. It sends the instance's secret to
the provider's service and spends one of the account's API calls. It does
this every time it is called, and a cached answer would be wrong.

It answers **200** with what the provider said, as a new `ProviderCheck`:

| Field | Meaning |
| --- | --- |
| `outcome` | `ok`, `refused` (the service answered and refused: a key it does not accept, a region not enabled, a host without the hypervisor), or `unreachable` (nothing answered). |
| `message` | The provider's error, as the pool would have shown it; absent on `ok`. |
| `checkedAt` | When the check ran. |

A failed check is the route's answer, not the route's failure. A problem
response (404, 403, 500) means the server could not run the check.
`unreachable` and `refused` stay apart, because each has a different fix.
`unreachable` means the server's network or proxy, and `refused` means the
instance's config.

The route bounds the check at 60 seconds. A check still running then is
`unreachable`. A disabled instance is still checked, so an operator can check
it before enabling it.

### 2. The result is not stored

No health field is added to `SandboxProviderInstance`, and nothing checks an
instance in the background. The answer is true when it is given, and only
the caller can say when it wants one.

### 3. Every runtime answers it: `Check` is required, at each layer

- `sandbox.Provider` gains `Check(ctx context.Context) error`.
  `poolruntime.Provider` delegates it.
- `poolruntime.RuntimeProvider` gains `Check(ctx) error`.
  - `discovm.Runtime` calls its engine's `Driver.Check`, the call
    `poolMachine.create` already makes. That is boxd's `Whoami` and region
    listing, archil's account, and the host hypervisor for a held driver.
  - `dockerworker.Engine` calls its driver's check.
- `dockerworker.Driver` gains `Check(ctx) error`. Each driver checks what its
  first `EnsureVM` would need first, and makes nothing:
  - `docker`: a ping of the Docker daemon it is pointed at.
  - `digitalocean`: a read of its API's account with its token.
  - `exec`: the command run as `<command> check`, with no pool id. A non-zero
    exit refuses, with stderr as the message. A script written before this op
    fails the check with its own usage error. Only a check call runs it, so
    such a script's pools are unaffected.
  - `libkrun`, `vz`, `wslc`: the host prerequisites the driver's VM needs.
    These drivers have no service to reach, so they never answer
    `unreachable`.

An error that wraps `sandbox.ErrPoolNotReachable` or
`sandbox.ErrProviderNotReachable` is `unreachable`; any other error is
`refused`. Each runtime already marks a service that does not answer with the
first: `discovm`'s `notReachable` does, and the engine's readiness waits do.

*Amended:* the `docker` driver marks the dial errors from its own transport
with the second. Its daemon is pinged when the provider is built, and a build
error reaches every resolver of the instance. The sandbox paths read
`ErrPoolNotReachable` as a pool coming back and wait minutes on it, so a
daemon that is down would turn an immediate failure into a long wait.
`ErrProviderNotReachable` is read by the check alone. Marking the build's
ping `ErrPoolNotReachable` (that wait) and leaving it unmarked (a daemon down
when the provider was not yet built would read `refused`, pointing at the
config) were both rejected.

The check goes through the same instance the server resolves for the
instance's pools. That means the same joined secret and the same
endpoint-policy dialer (26-11-11-080 §§3, 8). A check that passes has used
the same route the pool's create would use. *Amended:* an enabled instance is
checked on the provider `ProviderManager.ResolveInstance` keeps for it. A
disabled one is checked on a provider built for the check alone, never
initialized or cached, and closed when the check returns. Nothing else builds
a disabled instance: initializing marks its pools, and a build may start the
provider's own loops.

### 4. The CLI and the live run use it

`discobox admin provider check <provider>` calls the route. It prints the
outcome and the message, and exits non-zero unless the outcome is `ok`.
`scripts/live-run.sh` runs it after the provider is created and before the
pool is made. On anything but `ok`, the script reports
`| FAIL | preflight |` with the message, removes the provider, and stops.

### 5. An expired attempt names what it was waiting on

When a never-registered attempt's reconcile ends in `ErrPoolNotReachable`,
the reconciler records the error on the attempt (`PoolBootstrap.Waiting`, in
the pool's existing `bootstrap` JSON column, so no schema change). It is
written only when the error changes. It is display, not work evidence: it
renews no budget and does not end the attempt (306 §2). When the attempt
expires, the expiry pass's failure carries it:
`pool … bootstrap …: bootstrap stopped making progress (last waiting on: …)`.
A pool that fails this way says why, even when no one ran the check.

## Not decided here

- Whether the live run, when given no `--harness`, uses `claude-code` or
  refuses at preflight because the project has no default harness. Either one
  meets the issue. The implementation chooses; this needs no ADR.
- The verify-recipes live-run text about starting the run's server with the
  box's proxy environment is a docs change and is delivered with this one.
- Renaming `ErrPoolNotReachable` now that a check without a pool reads it is
  left to implementation.

## Alternatives rejected

- **No route: rely on §5 alone.** The pool would name its cause, but only
  after it was made and its two-minute silence budget had run out. The issue
  asks for a stop at preflight, before anything is made.
- **Check on provider create, and refuse an instance that fails.** That ties
  a durable write to whether the network is up at that moment. An operator
  may configure a provider before its network or key is live. A refused
  create also hands back nothing to fix: the config was valid. Create keeps
  checking only the config, and the caller who wants a check asks for one.
- **A health field on the instance, refreshed in the background.** It would
  be stale whenever it is read. It would spend provider API calls on every
  instance whether or not anyone is looking, and it would put a service's
  answer into the instance's durable row.
- **Answer a failed check with a problem response (502/503).** That mixes up
  the server failing to run the check with the provider's answer, and the
  CLI and preflight must tell the two apart.
- **`GET`.** The check sends a secret out and spends a call. It is neither
  safe nor cacheable.
- **An optional `Checker` interface, implemented by the runtimes that have a
  service.** Every runtime has something its first create needs and can
  check, so the method is required, at every layer.
- **Extend `Provider.Status()`.** It describes a type in the catalog, takes
  no context, and is read synchronously when the catalog is listed. A call
  out to a service has no place there.
- **Have the preflight make a throwaway pool.** That makes and pays for the
  very thing the preflight is supposed to come before, and it fails the same
  slow way.

## Consequences

- The API gains one route and one schema, and the CLI one command. The
  generated clients and the handler tests move with them.
- Every `dockerworker.Driver` implements `Check`, test fakes included. The
  `exec` command contract gains the `check` op, documented in the package.
- A pool's error says what its attempt was waiting on.
- `go test ./test/liverun` covers a provider the server cannot reach (the
  preflight stops before `admin pool create`, naming the error) and a
  project with no default harness.
