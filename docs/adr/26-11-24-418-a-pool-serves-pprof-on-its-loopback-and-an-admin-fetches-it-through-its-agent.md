# 26-11-24-418 — A pool serves pprof on its loopback, and an admin fetches it through its agent

- **Status**: Proposed
- **Date**: 2026-11-24
- **Issue**: [#1752](https://github.com/discobox-ai/discobox/issues/1752)
- **Relates to**: [26-11-18-571](26-11-18-571-a-server-routes-path-says-who-answers-it-and-nothing-is-under-api.md),
  where the admin route goes;
  [0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md)
  §4, the agent relaying a loopback API of the proxy's;
  [26-11-23-004](26-11-23-004-a-snapshot-build-reports-its-progress-and-an-admin-reaches-it-through-its-pool.md)
  §4, an admin reaching into a pool through its agent;
  [26-11-11-963](26-11-11-963-one-vm-provider-type-every-pool-runs-a-linux-pool-agent-in-a-machine-its-driver-makes.md),
  every pool's agent runs in a Linux container or machine of its own.

## Context

At `928a93b4`:

- **The server can be profiled; the pool can't.** `server/internal/server/pprof.go`
  holds `startPprof`, `writePprofToken`, `requirePprofToken` and
  `pprofHandler`. Its address check is `validatePprofListen`, with `PprofOff`,
  in `server/internal/config/config.go`. `server/DESIGN.md` "Runtime
  Observability" describes it, and `task dev:pprof` fetches from it. Nothing
  under `pool-agent/`, `proxy/` or `pkg/` imports `net/http/pprof` or
  `runtime/trace`.
- **One binary, three long-lived processes.** `discobox-pool-agent` runs the
  agent (`RunAgent`), the proxy (`proxy`, `proxyagent.RunProxy`, which hosts
  `proxy/` and `dnsforward`) and the BuildKit mediator (`buildkit-mediator`),
  each as its own systemd unit (`pool-agent/bundle/features/*/systemd/`), all
  as root.
- **How settings reach them.** The agent reads `Bootstrap` (`Load`,
  `pool-agent/bootstrap.go`), which the server renders from the pool
  (`server/providers/poolruntime/pool.go`). The proxy and mediator units both
  read `/etc/discobox/proxy.env`, which the agent writes at every start
  (`proxyagent.WriteUnitEnvironment`). A pool setting that travels this way
  already exists. `disableAgentEndpointTls` is a column on `model.Pool`, set
  by `admin pool create/update`, and carried in `Bootstrap`. Changing it places
  the pool's host again (`resources/pools/service.go`).
- **The pool's loopback is the pool's.** The proxy's control API is on
  `127.0.0.1:17084` (`proxyagent.ControlListenAddress`) and its origins on
  `127.0.0.1:17086`, and both are kept from sandboxes by being on loopback.
  The proxy refuses any destination that resolves to its own loopback
  (`proxy/dial.go`, `errOwnLoopback`), so a sandbox can't reach that loopback
  through its egress either. Fixed ports in use on a pool: 17080, 17081
  (mediator, on every interface), 17084, 17086 and 17090 (tunnels).
- **The control plane already reaches loopback APIs through the agent.** The
  audit routes (`audit/http`, scope `audit:read`) are server routes under
  `/projects/{p}/pools/{pool}/…`. The server forwards them to the pool agent,
  which calls the proxy's loopback control API with a token it signs
  (`pool-agent/DESIGN.md` "Reading the Proxy's Audit"). `discobox admin pool
  console` reaches a pool's host through the provider driver instead. That
  host is the Docker host's own namespaces on a Docker pool (not the pool
  container's network), a login shell on a remote VM driver, and nothing on a
  local one.

## Decision

### 1. One shared package, used by the server and the pool

`startPprof`, `writePprofToken`, `requirePprofToken`, `pprofHandler`,
`validatePprofListen` and `PprofOff` move into `pkg/pprofserve`. The server's
`Run` and `config.Load` call it directly, and their behaviour, flags, defaults
and error text don't change. Nothing wraps it.

The design carries over whole. The listener is opt-in, on a loopback
`host:port` checked both as configured and as bound. It has its own
`http.Server` and mux, and never `http.DefaultServeMux` or any router,
channel, tunnel or proxy listener. A fresh random token goes to a 0600 file on
every start, is removed on stop, and is required as every request's basic-auth
password. There is no `WriteTimeout`, stop calls `Close`, and a bind failure
fails the process's start.

### 2. A pool turns it on with one field, and each process has a fixed port

`model.Pool` gains `profiling` (bool, default false, a migration that adds the
column as `false`), with the same API shape as `disableAgentEndpointTls`: set on
`admin pool create` and `admin pool update`, carried in `Bootstrap` as
`profiling`. A change places the pool's host again, as that field's does. A
pool made before this has it off.

When it's on, each process listens on its own fixed loopback port:

| Process | Address |
| --- | --- |
| agent | `127.0.0.1:17091` |
| proxy | `127.0.0.1:17092` |
| BuildKit mediator | `127.0.0.1:17093` |

The agent reads the flag from its bootstrap. It writes one variable into
`proxy.env` for each of the two units (proxy, mediator), naming that process's
address, so neither unit reads the bootstrap for it. The addresses are
constants beside `ControlListenAddress`, not settings. The pool's loopback is
its own, in its container's network namespace or its own machine, so nothing
else on it competes for a port, and a fixed port is what lets the agent find a
process without being told (§3).

### 3. An admin fetches a profile through the pool's agent

The server adds a hand-wired, streaming admin route under the pool, beside
`console`, `logs` and `audit` (ADR 26-11-18-571 §1):

```
GET /projects/{p}/pools/{pool}/pprof/{process}/{profile}?seconds=N
```

`process` is `agent`, `proxy` or `mediator`. `profile` is any name the standard
set serves: `profile` (CPU), `trace`, `heap`, `allocs`, `goroutine`, `block`,
`mutex`, `threadcreate`. The route takes the same authorization as the pool's
console. The server forwards it to a new pool-agent route, signed with a new
scope, `pool:pprof`, that nothing else carries. It keeps nothing.

The pool agent answers it as a relay. It reads the named process's token file,
calls that process's fixed loopback address with it, and streams the answer
back unchanged. The pprof handlers stay off the agent's router. The relay is
the only thing on it, and it reaches the agent's own profile the same way it
reaches the proxy's. A pool with `profiling` off answers 404 with a problem
body that says profiling is off for the pool. A process that doesn't answer
gives 502.

The CLI gains `discobox admin pool pprof [POOL] --process agent|proxy|mediator
[--profile NAME] [--seconds N] [-o FILE]`, which writes the profile to a file.
`task dev:pool-pprof` (`POOL`, `PROCESS`, `PROFILE`, `SECONDS`) calls it, then
opens the file in `go tool pprof`, as `dev:pprof` does for the server. The
dev loop makes its default pool with `profiling` on.

This works on every runtime. It needs no provider console, no `docker exec`
and no VM exec path.

### 4. Every request still needs the token, even though no sandbox reaches the pool's loopback

How each driver keeps a sandbox off the pool's loopback:

| Pool | Why a sandbox can't reach the pool's `127.0.0.1` |
| --- | --- |
| Docker (local daemon, shared daemon, Docker Desktop) | The pool is its own container. Its sandboxes are sibling containers on the pool's internal network (`proxyagent.SandboxNetworkName`), each in its own network namespace, so loopback is not shared. On Docker Desktop, `host.docker.internal` leads to the host's loopback, which is why the server needs the token, and never to the pool container's. |
| A pool VM running the Docker runtime | The same, inside the pool's VM. |
| Machine pools (boxd, archil) | Each sandbox is a machine of its own. A packet for 127/8 from another host is not delivered. |
| Every pool | A sandbox's egress goes through the pool's proxy, which refuses its own loopback as a destination (`errOwnLoopback`). |

The token is kept anyway, and its files are 0600 and owned by root. A relay or
port forward added later could reach loopback without anyone noticing, and a
profile shows every goroutine's stack and the command line, and a long one
stalls the process. Implementation checks that no guest relay or vsock
forward maps a port onto 17091–17093. A test asserts that the proxy refuses
them as destinations.

### Open, for implementation

- The token files' exact paths. They go under the pool's `layout.Root`
  state, one per process, through a `layout` helper the agent and the two
  units share. The name is left to implementation.
- The names of the two `proxy.env` variables.

## Alternatives rejected

- **A `DISCOBOX_*` variable on the pool's box only.** This would add nothing to
  the API or the store. But the control plane couldn't set it: only a pool whose box someone can
  edit files in could be profiled. The owner chose the pool field (2026-11-24).
- **A dev-only task over `docker exec` or the VM's exec path.** This would add
  nothing to the agent's router. But it works only in the dev loop and needs a
  different path for each runtime. A local VM pool has none today, and the
  Docker pool console runs in the host's network namespace, not the pool's.
  The owner chose the admin route (2026-11-24).
- **Serving the pprof handlers on the agent's router, behind `pool:pprof`.**
  This would mean one listener fewer. But the profile would then share the
  router's transports and timeouts, which the server's design rules out. And
  it wouldn't reach the proxy or the mediator, which would need relaying
  anyway.
- **One base port with fixed offsets.** It is the same three fixed ports with
  one more setting. Nothing on a pool's loopback competes for them (§2).
- **A configurable address per process.** That adds a setting each for three
  processes, for no use: the relay is the only intended client, and it has
  to know the address.

## Consequences

- The server's pprof code moves to `pkg/pprofserve`, and its tests move with it.
- The pool API, model, store migration, bootstrap, CLI and OpenAPI gain
  `profiling` and the `pprof` route. User-facing text describes the
  behaviour, not this ADR.
- An agent that is wedged can't relay its own profile. `admin pool console`
  (on a Docker or remote VM pool) is still the fallback, and from there
  `nsenter` into the pool's network namespace or a shell in the machine
  reaches the listener with the token.
- A long CPU profile or trace streams through the control-plane channel for
  its whole length. The relay sets no deadline shorter than `seconds` allows.
- `pool-agent/DESIGN.md` gets a "Runtime Observability" section that links to
  the server's, and `proxy/DESIGN.md` notes that the proxy process can be
  profiled.
