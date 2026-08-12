# sshd Design

`internal/sshd` is the SSH control-plane ingress ADR 0024 describes: a real
SSH server (`golang.org/x/crypto/ssh`) listening on `DISCOBOX_SSH_LISTEN`
that maps SSH session channels onto the existing exec primitive and serves
`direct-tcpip` through a new sandbox-agent endpoint. The sandbox runs no
`sshd` — every SSH primitive here is translated into the same `execs`
primitive and `execstream/frame` codec the CLI already drives.

## Why this package authenticates independently of `internal/auth`

`internal/auth`'s `Authentication`/`Authorization` middleware chain is HTTP
middleware: it reads an `*http.Request`. sshd has no HTTP request — a
connection's identity is decided once, during the SSH key exchange's
`PublicKeyCallback`, before any channel (let alone any HTTP-shaped request)
exists. So sshd does not register an `auth.Authenticator`/`Authorizer`; it
builds an `auth.Principal` directly in `publicKeyCallback` and carries it on
the connection's `context.Context` via `auth.WithPrincipal`, then calls
`services.SandboxService.AcquireSandboxHTTPClient` — the exact same choke
point `internal/server`'s hand-wired HTTP proxies call — for every
session-channel attach and every `direct-tcpip` dial. This is what makes ADR
0024's "does not skip authorization" true without new authorization logic:
the scope check inside `AcquireSandboxHTTPClient` cannot tell an SSH-derived
principal from an HTTP one.

## Two authorized-key layers

- `<data dir>/authorized_keys` — a plain `authorized_keys(5)` file
  (`authorizedkeys.go`), reloaded on every connection attempt (not cached),
  so editing it takes effect without a restart. A match authenticates as the
  server's default user with `Scopes: [auth.ScopeAll]`.
- Project-scoped `model.SSHKey` rows (`server/internal/resources/sshkeys`) —
  a match authenticates as the enrolling user (`SSHKey.CreatedBy`) with
  `Scopes: [exec:read, exec:write, tcp:connect]`, valid only for that
  project's sandboxes.

`publicKeyCallback` checks the file layer first; a key present in both
layers gets the broader (file) grant, per ADR 0024 §5. The grant and
resolved project/sandbox IDs travel from `publicKeyCallback` to the
post-handshake connection handler via `ssh.Permissions.Extensions` — the
only channel `x/crypto/ssh` provides for this.

## Username routing

`route.go`'s `ResolveUsername` implements ADR 0024 §1's two forms, in this
precedence:

1. `sbx_<id-or-prefix>` — always parsed as a bare sandbox ID/prefix, even if
   it contains a literal `.`, resolved across all projects via
   `store.FindSandboxByIDPrefix` (a project-agnostic sibling of
   `store.GetSandbox`, needed because project membership isn't known before
   the username is parsed).
2. `<sandbox>.<project>` — split on the *last* `.`. The project component
   resolves by exact slug, then exact name, then `id.ResolveShort` against
   project IDs; the sandbox component resolves by `id.ResolveShort` against
   that project's sandbox IDs only (not by display name). Both reuse the
   root `id` package's prefix-match rules, the same ones the CLI uses for
   short-ID arguments.

Ambiguous or zero matches in either form is a hard resolution failure that
never distinguishes "no such sandbox" from "no such project" on the wire —
an unauthenticated connection attempt must not learn what exists.

## Session channel → exec mapping (ADR 0024 §2)

`session.go`'s `sshSession` tracks pty-req/env state per channel and
dispatches `shell`/`exec`/`subsystem` (only one legal per channel) to
`attach`, which calls `AcquireSandboxHTTPClient`, POSTs a
`CreateSandboxExecRequest` to the pool-agent target
(`sandboxagentclient.TargetURL`), dials the exec's attach websocket
(`dial.go`'s `dialFrameWebSocket`, shared with the TCP tunnel below), and
only then POSTs `/start`. Every session channel requests `workdir: "~"`: SSH
starts a session in the user's home directory and `scp`/`sftp` resolve
relative paths against it, while the sandbox's own exec default is the primary
source directory — right for `disco shell`, but it would land uploads inside
the sandbox's git working tree. The tilde is expanded in the sandbox
(`sandbox-agent/execs`), the only place that knows the run user's home.
That order is load-bearing and matches the CLI's:
an exec is created suspended, and a fast command broadcasts its output as it
exits, so starting before the attach is open races the exec to its own
output — while never starting leaves the session hanging on a suspended exec.
Two pump goroutines then bridge the SSH channel and the exec's frame stream:
`Input`/`CloseInput` one way, `Stdout`/`Stderr`/`Exit` the other.
`exec "cmd"` needed a primitive extension — `CreateRequest.ShellCommandLine`
in `sandbox-agent/execs` — because SSH's `exec` carries one opaque
command-line string and sshd, running outside the sandbox, cannot resolve a
login shell path itself; see `sandbox-agent/DESIGN.md`.

A session that cannot be started is reported as one that ran and failed:
accept the request, write the reason to the channel's stderr, and exit
`sessionSetupExitStatus` (255, ssh's own convention for "the session never
ran"). Refusing the request instead is what produces `shell request failed on
channel 0` with the cause reaching only the server log —
`SSH_MSG_CHANNEL_FAILURE` has no message field, and writing stderr *before*
refusing does not help either, because OpenSSH discards extended data on a
refused request. That was verified against the real client, which printed
nothing; the Go client happened to show it, which is exactly the kind of
difference a unit test alone would have blessed.

A **subsystem** is still refused outright. Its client is waiting to speak a
protocol rather than to read prose, and `sftp` reports the refusal legibly by
itself.

What the reason may say depends on which failure it is.
`AcquireSandboxHTTPClient` answers both "may this connection reach this
sandbox" and "is the sandbox reachable at all". The first must not distinguish
"no such sandbox" from "not yours" (ADR 0024 §1), so 401/403/404 collapse into
one generic message; everything else — a pool that is not active, a sandbox
that cannot start — says what happened, because reporting it as an
authorization problem sends the reader to look at keys and grants for something
that is neither.

Exit always sends SSH's `exit-status`, never `exit-signal`: the shim already
converts a signal death to the shell convention (128+signum), so
`frame.ExitPayload` never carries a bare signal name to build an
`exit-signal` message from.

## `direct-tcpip` (ADR 0024 §3–4)

`tcpip.go`'s `handleDirectTCPIPChannel` unmarshals the RFC 4254 §7.2
payload, authorizes with `tcp:connect`, and dials the sandbox-agent's new
`/tcp/attach` endpoint via the same `dialFrameWebSocket` helper session
channels use — reusing `execstream/frame`'s `Input`/`Stdout`/`CloseInput`
rather than raw bytes is specifically what lets a half-close cross this
tunnel (a websocket has none of its own; TCP does, and `rsync`-shaped
protocols depend on it). A dial failure on the sandbox-agent side rejects
the channel before accepting, matching what a real `sshd` does for a
refused `-L` target. See `sandbox-agent/DESIGN.md` for the sandbox-side
dial and pump, and `pool-agent/DESIGN.md` for the proxy route.

## Two front doors, one server

`sshd.Server` is fed by two things: the optional TCP listener
(`DISCOBOX_SSH_LISTEN`), and `GET /ssh/connect`, which accepts a websocket and
hands `websocket.NetConn`'s byte stream to the same `handleConn`. Both
authenticate identically, because authentication is inside the SSH protocol.

That is what lets `disco tools ssh` work against a server that binds no SSH
port: reaching the server the way the CLI already reaches it needs no new
machine-wide surface. The route is exempt from HTTP auth for the same reason
the TCP listener needs none — SSH authenticates by public key before any
channel exists, and an HTTP credential in front of it would only be a second
lock on the same door.

So `GET /ssh` answers two different questions. `enabled` is whether this server
can serve SSH at all, which is always true; `address` is the advertised TCP
endpoint, which is absent when none is configured. A client that needs a
persistent `ssh_config` needs the address; `disco tools ssh` needs only
`enabled` and the host key.

## Endpoint discovery

The endpoint and host key are served together by `GET /ssh` (an ordinary
generated handler over `services.SSHIngress`, resolved in `internal/server`
before the router is built), so `disco box ssh-config` hard-codes neither. The
address it reports is the *advertised* one, never the bind address — see
[`server/DESIGN.md`](../../DESIGN.md#listen-endpoints).

## Persistent host key

`hostkey.go`'s `LoadOrCreateHostKey` writes an ed25519 key once to
`<data dir>/ssh_host_ed25519_key` and never regenerates it implicitly —
rotating it breaks every enrolled client's `known_hosts` entry. Creation is
write-to-temp-then-`Link`, not a direct `O_CREATE|O_EXCL` write: `O_EXCL`
makes *creation* atomic but not the write that follows it, so a concurrent
reader could see a truncated file mid-write. Writing the complete key to a
temp file first and only then hard-linking it into place means the
destination is always either absent or complete. The loser of a concurrent
generation race reloads the winner's bytes rather than trusting its own,
mirroring `poolagent/auth.go`'s `EnsureTrustKey` idiom for a DB
unique-constraint race, adapted to a filesystem one.

## Auto-start

Inherited for free. Every session-channel attach and `direct-tcpip` dial
goes through `AcquireSandboxHTTPClient` → the pool-agent's proxy routes,
which already wrap sandbox-directed handlers in `autoStart`
(`pool-agent/server/autostart.go`); the new `/tcp/attach` proxy route reuses
that wrapper unchanged (ADR 0024 §7).

## Remote forwarding is out of scope by omission, not by a check

`handleConn` replies `false` to every global SSH request. This is the
entire implementation of ADR 0024 §8: `tcpip-forward`/`cancel-tcpip-forward`
are simply never among the recognized request types, so they fall through
to the same blanket refusal every other unrecognized global request gets.
No `-R`-specific code exists anywhere in this package.
