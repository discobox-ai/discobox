# sshd Design

`internal/sshd` is the SSH control-plane ingress ADR 0024 describes: a real
SSH server (`golang.org/x/crypto/ssh`) that maps SSH session channels onto the
existing exec primitive and serves `direct-tcpip` through the sandbox-agent's
`/tcp/attach` endpoint. The sandbox runs no `sshd` — every SSH primitive here
is translated into the same `execs` primitive and `execstream/frame` codec the
CLI already drives.

It binds no listener. The one way in is `GET /ssh/connect` on the API router
(ADR 0057); there is no `Serve(net.Listener)` and no port to configure.

## Why this package authenticates independently of `internal/auth`

`internal/auth`'s `Authentication`/`Authorization` middleware chain is HTTP
middleware: it reads an `*http.Request`. sshd has no HTTP request — a
connection's identity is decided once, during the SSH key exchange's
`PublicKeyCallback`, before any channel (let alone any HTTP-shaped request)
exists. So sshd does not register an `auth.Authenticator`/`Authorizer`; it
builds an `auth.Principal` directly from the callback's grant
(`principalFromPermissions`) and carries it on the connection's
`context.Context` via `auth.WithPrincipal`, then calls
`services.SandboxService.AcquireSandboxHTTPClient` — the exact same choke
point `internal/server`'s hand-wired HTTP proxies call — for every
session-channel attach and every `direct-tcpip` dial. This is what makes ADR
0024's "does not skip authorization" true without new authorization logic:
the scope check inside `AcquireSandboxHTTPClient` cannot tell an SSH-derived
principal from an HTTP one.

## Two authorized-key layers

- `<data dir>/authorized_keys` — a plain `authorized_keys(5)` file
  (`authorizedkeys.go`), reloaded on every connection attempt (not cached),
  so editing it takes effect without a restart. Malformed lines are skipped. A
  match authenticates as the server's default user with
  `Scopes: [auth.ScopeAll]`.
- Project-scoped `model.SSHKey` rows (`server/internal/resources/sshkeys`) —
  a match authenticates as the enrolling user (`SSHKey.CreatedBy`) with
  `Scopes: [exec:read, exec:write, tcp:connect]`, valid only for that
  project's sandboxes.

`publicKeyCallback` resolves the username first (a username that does not
resolve fails authentication), then checks the file layer; a key present in
both layers gets the broader (file) grant, per ADR 0024 §5. The grant, user
ID, and resolved project/sandbox IDs travel from `publicKeyCallback` to the
post-handshake connection handler via `ssh.Permissions.Extensions` — the only
channel `x/crypto/ssh` provides for this.

## Username routing

`route.go`'s `ResolveUsername` implements ADR 0024 §1's two forms, in this
precedence:

1. `sbx_<id-or-prefix>` — always parsed as a bare sandbox ID/prefix, even if
   it contains a literal `.`, resolved across all projects via
   `store.FindSandboxByIDPrefix` (a project-agnostic sibling of
   `store.GetSandbox`, needed because project membership isn't known before
   the username is parsed).
2. `<sandbox>.<project>` — split on the *last* `.`. The project component
   resolves by exact name (unique per owner), then `id.ResolveShort` against
   project IDs; the sandbox component resolves by `id.ResolveShort` against
   that project's sandbox IDs only (not by display name). Both reuse
   `github.com/discobox-ai/x/id`'s prefix-match rules, the same ones the CLI
   uses for short-ID arguments.

Ambiguous or zero matches in either form is a hard resolution failure that
never distinguishes "no such sandbox" from "no such project" on the wire —
an unauthenticated connection attempt must not learn what exists.

## Connection handling

`handleConn` runs the handshake, builds the principal, and dispatches channels
by type: `session` and `direct-tcpip` each get a goroutine; any other channel
type is rejected with `UnknownChannelType`. Global requests are handled
separately (see the last section).

## Session channel → exec mapping (ADR 0024 §2)

`session.go`'s `sshSession` records pty-req and env state per channel (env is
limited to `TERM`, `LANG`, and `LC_*`; others are silently dropped, and the
pty's terminal type fills `TERM` if the client sent none) and dispatches
`shell`/`exec`/`subsystem` (only one legal per channel) to `attach`:

- `shell` → a login-shell exec.
- `exec "cmd"` → a shell exec with `ShellCommandLine` set. SSH's `exec`
  carries one opaque command-line string and sshd, running outside the
  sandbox, cannot resolve a login shell path itself, so the sandbox runs its
  login shell with `-lc <cmd>` (`CreateRequest.ShellCommandLine` in
  `sandbox-agent/execs`; see `sandbox-agent/DESIGN.md`).
- `subsystem sftp` → an exec of `/usr/lib/openssh/sftp-server` (installed by
  the sandbox-agent image), never with a pty. Any other subsystem is refused.

`attach` calls `AcquireSandboxHTTPClient` with `exec:read`/`exec:write`, POSTs
a `CreateSandboxExecRequest` to the pool-agent target
(`sandboxagentclient.TargetURL`), dials the exec's attach websocket
(`dial.go`'s `dialFrameWebSocket`, shared with the TCP tunnel below), and
only then POSTs `/start`. That order is load-bearing and matches the CLI's:
an exec is created suspended, and a fast command broadcasts its output as it
exits, so starting before the attach is open races the exec to its own
output — while never starting leaves the session hanging on a suspended exec.

Every session channel requests `workdir: "~"`: SSH starts a session in the
user's home directory and `scp`/`sftp` resolve relative paths against it,
while the sandbox's own exec default is the primary source directory — right
for `discobox shell`, but it would land uploads inside the sandbox's git
working tree. The tilde is expanded in the sandbox (`sandbox-agent/execs`),
the only place that knows the run user's home.

Once attached, `pump` bridges the SSH channel and the exec's frame stream:
channel data and EOF become `Input`/`CloseInput` one way, and
`Stdout`/`Stderr`/`Exit` come back the other (an `Error` frame closes the
channel). `window-change` and `signal` requests become `Resize` and `Signal`
frames on the same attach connection.

A session that cannot be started is reported as one that ran and failed:
accept the request, write the reason to the channel's stderr, and exit
`sessionSetupExitStatus` (255, ssh's own convention for "the session never
ran"). Refusing the request instead is what produces `shell request failed on
channel 0` with the cause reaching only the server log —
`SSH_MSG_CHANNEL_FAILURE` has no message field, and writing stderr *before*
refusing does not help either, because OpenSSH discards extended data on a
refused request. The Go client does show it, so a unit test against the Go
client alone cannot catch this; the behavior is pinned to what the real
OpenSSH client prints.

A **subsystem** is still refused outright. Its client is waiting to speak a
protocol rather than to read prose, and `sftp` reports the refusal legibly by
itself.

What the reason may say depends on which failure it is.
`AcquireSandboxHTTPClient` answers both "may this connection reach this
sandbox" and "is the sandbox reachable at all". The first must not distinguish
"no such sandbox" from "not yours" (ADR 0024 §1), so `acquireFailureReason`
collapses 401/403/404 into one generic message; everything else — a pool
that is not reachable, a sandbox that is being deleted, an exec that cannot be
created or started — says what happened, because reporting it as an
authorization problem sends the reader to look at keys and grants for
something that is neither. sshd uses the fail-fast `AcquireSandboxHTTPClient`,
not `AwaitSandboxHTTPClient` (ADR 0039): an unreachable pool fails the session
with its reason rather than waiting.

Exit always sends SSH's `exit-status`, never `exit-signal`: the shim already
converts a signal death to the shell convention (128+signum), so
`frame.ExitPayload` never carries a bare signal name to build an
`exit-signal` message from.

## `direct-tcpip` (ADR 0024 §3–4)

`tcpip.go`'s `handleDirectTCPIPChannel` unmarshals the RFC 4254 §7.2
payload, authorizes with `tcp:connect`, and dials the sandbox-agent's
`/tcp/attach?host=&port=` endpoint via the same `dialFrameWebSocket` helper
session channels use — reusing `execstream/frame`'s
`Input`/`Stdout`/`CloseInput`/`CloseOutput` rather than raw bytes is
specifically what lets a half-close cross this tunnel (a websocket has none of
its own; TCP does, and `rsync`-shaped protocols depend on it). An
authorization or dial failure rejects the channel before accepting it
(`ConnectionFailed`), matching what a real `sshd` does for a refused `-L`
target.

`pumpDirectTCPIP` ends each direction on its own, because a forwarded
connection's two halves do. `CloseInput` carries the client's EOF to the
target; `CloseOutput` carries the target's back and becomes `CloseWrite` on the
SSH channel, leaving the other half open. Only the attach connection ending
closes the channel — once it is gone nothing can be delivered either way, and
closing is also what unblocks the reader. `Error` frames are logged rather than
dropped: a tunnel that failed inside the sandbox must not look like one that
simply ended. `tcpip_concurrency_test.go` covers what a forwarded connection is
actually asked to do (a VS Code Remote-SSH session is the demanding case):
sixteen channels at once, four megabytes through one, and a remote half-close.
See `sandbox-agent/DESIGN.md` for the sandbox-side dial and pump, and
`pool-agent/DESIGN.md` for the proxy route.

## One front door

`sshd.Server` is fed by one thing: `GET /ssh/connect` (`connect_route.go`'s
`RegisterConnectRoute`), which accepts a websocket and hands
`websocket.NetConn`'s byte stream to `handleConn`. `internal/server` registers
it after `NewApp`, since sshd needs the services the app builds, on the same
router as the API.

There is no TCP listener (ADR 0057): a port to configure, publish, and
firewall would buy nothing the route does not already provide. SSH is
reachable wherever the API is, which is the property every client needs:
`discobox tools ssh` splices a loopback port onto this route, and a persisted
`ssh_config` reaches it through a `ProxyCommand` that runs `discobox admin
ssh-proxy` — which is how every tool built on the `ssh` binary rather than on
our client gets in: VS Code Remote-SSH, `scp`, `git`. See `cli/DESIGN.md`.

The route is exempt from HTTP auth (`auth.IsPublicPath`): SSH authenticates by
public key inside its own protocol, before any channel exists, and an HTTP
credential in front of it would only be a second lock on the same door.

## Host key discovery

`GET /ssh` answers one question — what host key to pin. It is an ordinary
generated handler (`handlers.GetSSHIngress`) over `services.SSHIngress`, whose
only field is `HostKey`; `internal/server`'s `resolveSSHIngress` loads it
before the router is built, so `discobox admin ssh-config` hard-codes nothing.
It is a public path because `ssh-config` reads it before any credential
exists. There is no address to advertise, because there is nowhere else to
dial, and nothing to enable, because every server serves it.

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
mirroring `internal/auth/poolagent/auth.go`'s `EnsureTrustKey` idiom for a DB
unique-constraint race, adapted to a filesystem one.

## Auto-start

Inherited for free. Every session-channel attach and `direct-tcpip` dial
goes through `AcquireSandboxHTTPClient` → the pool-agent's proxy routes,
which wrap sandbox-directed handlers — the exec routes and `/tcp/attach`
alike — in `autoStart` (`pool-agent/server/autostart.go`, ADR 0024 §7).

## Remote forwarding is out of scope by omission, not by a check

`handleConn` replies `false` to every global SSH request. This is the
entire implementation of ADR 0024 §8: `tcpip-forward`/`cancel-tcpip-forward`
are simply never among the recognized request types, so they fall through
to the same blanket refusal every other unrecognized global request gets.
No `-R`-specific code exists anywhere in this package.
