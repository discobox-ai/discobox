# 0024 — SSH is a control-plane ingress onto execs, and forwarded TCP terminates inside the sandbox

- **Status**: Accepted
- **Date**: 2026-08-06

## Context

Reaching a sandbox today means `disco`. The exec primitive behind it is already
general — `execs` takes `command`/`shell`/`harnessId`, `workdir`, `env`, `user`,
`tty`, `cols`, `rows`, and the attach stream carries
`Input`/`Stdout`/`Stderr`/`Resize`/`Signal`/`CloseInput`/`Exit` — but the only
thing that speaks it is our own client.

Everything else a developer owns speaks SSH. `scp` and `rsync` move files over
it, VS Code Remote-SSH and JetBrains Gateway attach over it, `git` clones over
it, and `ssh -L` is how people reach a dev server they just started. None of
that can be taught to speak `execstream/frame`, and none of it needs to be:
what those tools want from a host is a shell, a file transfer subsystem, and a
TCP tunnel, and two of the three already exist here under different names.

The gap is an ingress that speaks the protocol, plus the one primitive we
genuinely lack — an arbitrary TCP connection into the sandbox's network
namespace. The existing `/http/{port}` pool route reverse-proxies to the
container IP, which is HTTP-only and, more importantly, addresses the sandbox
from outside it.

## Decision

### 1. The SSH server is a control-plane ingress, not a per-pool listener

`discobox-server` grows an SSH listener (`server/internal/sshd`), configured by
`DISCOBOX_SSH_LISTEN`, conventionally `:3222`. Sandboxes are addressed through
the SSH username, which is the only routing field in the protocol available
before authentication completes: `ssh sbx_01H…@host` or
`ssh <sandbox>.<project>@host`, resolved by the same prefix-match rules the CLI
uses. `disco ssh-config` emits the matching `Host` and `known_hosts` entries.

The control plane is the only component that knows sandbox names, project
membership, and pool routing, so it is the only one that can turn a username
into an authorization decision. A listener on each pool agent would have to be
told all three, would multiply host keys and authorized-key distribution by the
number of pools, and would give a user a different endpoint per pool for what is
one logical fleet.

The SSH server reaches the sandbox over the existing chain — the same
`AcquireSandboxHTTPClient` lease, pool-agent route, and sandbox-agent endpoint
the HTTP API uses. It skips only the outer HTTP hop into itself, and does not
skip authorization: a session is created only after the connection's principal
has been authorized for the target sandbox exactly as an API caller would be.

### 2. A session channel is an exec; the sandbox runs no sshd

SSH session channels map onto the existing exec primitive, and nothing new is
added to the sandbox runtime for them:

| SSH | exec |
| --- | --- |
| `pty-req` | `tty: true`, `cols`/`rows`, `TERM` in `env` |
| `shell` | `shell: true` |
| `exec "cmd"` | `command` running the login shell with `-lc` |
| `subsystem sftp` | `command` running the image's `sftp-server` |
| `env` | `env`, restricted to `TERM`, `LANG`, `LC_*` |
| `window-change` | `Resize` frame |
| `signal` | `Signal` frame |
| channel EOF | `CloseInput` frame |
| stderr (non-TTY) | extended data type 1 |
| `Exit` frame | `exit-status` / `exit-signal` |

Running a real `sshd` inside each sandbox was rejected. It would be a second,
parallel way to start processes in a sandbox — with its own user database, its
own idea of the login environment, and its own lifecycle to supervise — and
every exec-level behaviour we already own (audit records, resource accounting,
reconnect, the shim's replay-on-attach) would have to be rebuilt inside it or
lost. It also puts a network-facing authentication surface inside the isolation
boundary, which is the wrong side of it.

The stderr row is exact rather than convenient: a TTY exec never emits `Stderr`
because the kernel already merged both streams onto the PTY, which is also what
an SSH client expects from a PTY session.

### 3. Forwarded TCP terminates inside the sandbox, not at the pool

`direct-tcpip` channels are served by a new sandbox-agent endpoint that dials
`host:port` from inside the sandbox and returns a byte pipe, reached through the
same server → pool-agent → sandbox-agent chain as execs, gated by a new
`tcp:connect` scope alongside `exec:read`/`exec:write`.

Terminating at the pool agent instead — dialing the container IP, as
`HTTPBaseURL` does — was rejected because it silently fails the common case.
`ssh -L 8080:localhost:3000` means localhost *inside* the sandbox, and a
process that bound `127.0.0.1:3000` is unreachable from the container IP.
Development servers bind loopback by default, so a pool-side implementation
would break precisely the workflow the feature exists to serve, and would break
it as a connection refused with no indication that the port was wrong rather
than the address. Only a dialer inside the network namespace resolves
`localhost` the way the user meant it.

This also generalizes `/http/{port}`, which stays as the browser-reachable
convenience route; the TCP endpoint is the primitive underneath the same idea.

### 4. The tunnel reuses `execstream/frame`

The TCP tunnel carries `Input`, `Stdout`, and `CloseInput` over its websocket
rather than raw binary messages.

Raw bytes were the obvious choice and are wrong: a websocket has no half-close,
and TCP does. `rsync` over a forwarded port, and any protocol where one side
signals "done sending" and then reads a reply, depends on a FIN that a
close-the-whole-socket transport cannot express. Reusing the existing codec
makes half-close expressible without inventing a second wire format, and the
frame types already mean exactly this.

### 5. Authorized keys are two layers with different meanings

- `<data dir>/authorized_keys` — an `authorized_keys(5)` file, server-wide.
  A key here authenticates as the server's default user and therefore reaches
  everything that user reaches. It is the operator's own key, and it is a file
  rather than an API because it must work before any API access exists.
- Project-scoped keys — a managed resource under a project, CRUD through the
  API and `disco ssh-key`. A key here authorizes only that project's sandboxes.

One layer was rejected in both directions. File-only cannot express "this key
reaches this project", which is the whole point of granting someone access
without granting them the server. API-only leaves no way in when the API is
what you are trying to reach, and no path for an operator to recover a server
whose only credential is inside it.

A key present in both layers is not an error: authentication resolves a
principal, authorization then decides, and the broader grant simply wins.

### 6. Key enrollment reads the local agent, and that is a convenience only

`disco ssh-key add` with no argument lists the public keys in the running
`SSH_AUTH_SOCK` agent, falling back to `~/.ssh/*.pub`, and asks which to
enroll. Nothing about this is a trust decision — listing an agent's public keys
proves nothing about possession of the private half, and enrollment is an
authenticated API call by a user who is already authenticated. It exists so
that enrolling a key is one prompt instead of a copy-paste, and no code path may
treat agent presence as evidence of anything.

### 7. Auto-start is inherited, not built

An SSH connection to a stopped sandbox starts it and waits. This requires
nothing new: the pool agent's `autoStart` wrapper already covers the
sandbox-directed routes, and `EnsureSandboxRunning` blocks until the
sandbox-agent answers (ADR 0017 §12). The new TCP route gets the same wrapper
for the same reason — a forwarded connection is demand for the sandbox exactly
as an exec is.

Archived sandboxes remain exempt and report the conflict, per ADR 0022 §5.

### 8. Remote forwarding is out of scope

`-R` and `tcpip-forward` are not implemented. `direct-tcpip` covers `-L` and,
being the same channel type, `-D` at no additional cost; `-R` needs a listener
opened inside the sandbox on demand and a reverse channel out, which is a
distinct mechanism serving a use we have not yet had. Revisit when a concrete
workflow needs a sandbox process to reach a service on the developer's machine
by port — until then, the proxy already handles outbound reach.

## Consequences

- `scp`, `rsync`, `git` over SSH, and VS Code Remote-SSH work against a sandbox
  without a Discobox client, because all of them reduce to a session channel, an
  `sftp` subsystem, and a TCP tunnel.
- The sandbox image must ship an `sftp-server` binary. It does not need `sshd`.
- The server holds a persistent host key in its data directory. Rotating it
  makes every enrolled client's `known_hosts` entry mismatch, so it is written
  once and never regenerated implicitly.
- A second authentication surface exists on the server, reachable before any
  HTTP authorization runs. Public-key only: no passwords, no
  keyboard-interactive.
- `direct-tcpip` reaches any port a sandbox process can reach, including
  loopback services never meant to be exposed. The authorization boundary is the
  sandbox, not the port: whoever may exec in a sandbox may already reach those
  services, so this grants no new reach — but it does make it convenient, which
  is worth stating rather than discovering.
