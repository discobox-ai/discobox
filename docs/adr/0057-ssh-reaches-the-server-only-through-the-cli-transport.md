# 0057. SSH reaches the server only through the transport the API answers on

Status: Accepted

Supersedes ADR 0024 §1's TCP listener.

## Context

ADR 0024 gave the control plane an SSH ingress and two front doors onto the same
`sshd`: a TCP listener on `DISCOBOX_SSH_LISTEN`, and `GET /ssh/connect`, which
hands a websocket's byte stream to the same `handleConn`. `disco tools ssh` used
the second — a loopback port opened for the life of the command — so an
interactive session never required the operator to hold a machine-wide SSH port
open.

`disco box ssh-config` could not use it. A persisted `ssh_config` has to name
something ssh can dial next week, and a loopback port that exists only while one
command runs is not that. So the emitted stanzas carried `HostName`/`Port` from
the server's advertised endpoint, and the command failed outright when the
server advertised none:

> this server has no SSH address to write a config for: set DISCOBOX_SSH_LISTEN
> to give it one, or connect with `disco tools ssh`

That split is the whole problem. Everything built on `ssh` rather than on a
terminal — VS Code Remote-SSH, JetBrains Gateway, `scp`, `rsync`, `git` over
ssh — reads `ssh_config` and drives the system `ssh` binary. None of them can be
handed a port that only exists inside a running `disco` process. So the door
that needs no server configuration served only our own client, and every tool
that motivated ADR 0024 in the first place was on the side that needed a port
opened, published, and firewalled correctly.

The immediate trigger was `disco tools vscode`, which opens a sandbox in VS Code
over Remote-SSH. It has no way to hand VS Code a host except by putting the host
in `ssh_config`.

## Decision

### 1. A persisted `ssh_config` carries a `ProxyCommand`, not an address

```
Host devbox devbox.discobox.internal sbx_01hq sbx_01hq.discobox.internal
    ProxyCommand '/usr/local/bin/disco' --server 'unix:///…/api.sock' box ssh-proxy
    User sbx_01hq
    IdentityFile …
    IdentitiesOnly yes
    HostKeyAlias proj_01hq.discobox.internal
    UserKnownHostsFile …
```

`disco box ssh-proxy` is a new hidden subcommand that splices its own stdin and
stdout onto `GET /ssh/connect`. It is the same dial `disco tools ssh`'s bridge
makes, per connection, with ssh owning the process instead of a loopback port.

Host keys are verified under `HostKeyAlias`, one name per project, rather than
under the address. A stanza with no address gives ssh nothing to derive a
`known_hosts` name from, and `known_hosts` needs one.

### 2. The TCP listener is removed

`DISCOBOX_SSH_LISTEN` and `DISCOBOX_SSH_ADVERTISE_ADDRESS` are gone, along with
`sshd.Server.Serve(net.Listener)`. `GET /ssh/connect` is the only front door.

Once every client reaches the ingress the same way, the listener is a second
implementation of "get bytes to `handleConn`" that no client needs and every
operator has to reason about: a port to allocate, publish through whatever is in
front of the process, and firewall — plus, on Windows, a firewall prompt on
first run. Keeping it "for the operators who want a direct port" would keep all
of that cost for a path nothing exercises.

`GET /ssh` therefore reduces to the host key. `address` described only the
removed listener, and `enabled` was already constant-true — every server can
serve SSH over its own API transport — so both are dropped rather than left as
fields whose only possible value is a lie waiting to happen.

## Alternatives rejected

**Keep the direct stanzas and teach `tools vscode` to hold a bridge open.** The
CLI would open its loopback port, write a stanza naming that port, launch the
editor, and stay in the foreground holding both. This was the first design, and
the reason it loses is the lifetime: Remote-SSH reconnects on its own — after a
laptop sleeps, after a network change, when the editor reopens the folder
tomorrow — and every one of those reconnects would find a dead port and a stanza
that lies. Keeping the CLI open for as long as the editor might want to
reconnect means keeping it open indefinitely, and closing it silently breaks a
window that looks fine. It also does nothing for `scp`, `git`, or a second
editor: each would need its own held-open command.

**A stanza per session in a globbed `Include` directory.** Same lifetime
problem, plus stale files to reap after a crash.

**Keep the listener as an opt-in fast path.** Tempting, because a direct TCP
connection is one process and one hop cheaper, and `--host`/`--port` could have
kept emitting direct stanzas for anyone who set it up. Rejected because the
failure it introduces is invisible and delayed: a config written on a machine
that could reach the port keeps working until the network changes around it, and
nothing re-renders the file to notice. One transport that always works beats two
that mostly do — and a second transport that is never the default is a second
transport nobody tests.

**Keep `SSHIngress.enabled` for a future kill switch.** There is no way to turn
SSH off today and no request for one. A field that only ever reports `true` is
read as a real distinction by whoever finds it next; if a kill switch is ever
wanted, it can be added then, meaning what it says.

## Consequences

- Every SSH connection through a written config spawns a `disco` process for its
  lifetime. That process is doing an `io.Copy` in each direction; the cost is a
  process, not throughput.
- The `ProxyCommand` records the absolute path of the executable that wrote it
  and the `--server` endpoint it was pointed at. Moving or deleting the binary
  breaks the config until it is rewritten, which is the same relationship the
  file already had with the key it names.
- A local (`unix://`) endpoint means ssh can auto-start the server the same way
  any other `disco` command does — which is what makes `ssh devbox` work from a
  cold machine.
- Reaching a sandbox by SSH now requires the `disco` binary on the client
  machine. It already did in practice — the config, the key, and the enrollment
  all come from it — but a hand-written `ssh_config` against a published port is
  no longer possible.
- `GET /ssh` is a breaking response change: a client reading `enabled` sees it
  absent and must be updated alongside the server.
- The `ssh.bats` suite dials through the `ProxyCommand` rather than a port, so
  what it exercises end to end is the path every real client takes.
