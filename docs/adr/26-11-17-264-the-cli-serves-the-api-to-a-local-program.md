# 26-11-17-264 — The CLI serves the API to a local program

- **Status**: Proposed
- **Date**: 2026-11-17

## Context

A program on a person's machine that wants the Discobox API — an admin web UI
first, then scripts and editors — has to do what the CLI does before it can
make one call: find the servers the person registered (`servers.json`,
[ADR 0119](0119-registered-servers-are-admin-remote.md)), settle which is the
primary ([ADR 0135](0135-the-primary-server-is-recorded-by-admin-remote-primary.md)),
reach each one over whatever its address names — a unix socket, HTTPS, or an
iroh peer with this machine's identity ([ADR 0116](0116-a-discobox-address-names-a-server-and-a-discobox.md)) —
and carry the token. All of that is in the CLI, behind `endpoint.HTTPClient`,
and a reverse proxy over it already exists for git (`endpoint.StartLoopbackProxy`).

## Decision

1. **`discobox admin api serve` serves every server's API on a local socket.**
   It runs until interrupted, and listens on a unix socket in the CLI's state
   directory (`api.sock`), mode 0600, unless `--listen` names another socket or
   a loopback `http://127.0.0.1:PORT`. A non-loopback address is refused.
2. **The socket's permissions are its access control.** Nothing on it
   authenticates a caller: whoever can open it acts as this user, with this
   user's identity and token, exactly as running the CLI would. That is why
   the default is a socket only the user can open, and TCP is loopback-only
   and never the default.
3. **`GET /v1/servers` lists the servers as the CLI would choose them now**:
   the primary first (the `--server` the command was started with, else the
   recorded primary, else the local server), then the registered ones in
   registration order, with a registered primary listed once under its name.
   The registry is read on every request, so `admin remote add`, `rm`,
   `rename` and `primary` are seen without a restart.
4. **`/v1/servers/NAME/api/PATH` is the API of the server listed as NAME**,
   reached through the CLI's client for its address, kept for the life of the
   command (a client per request would leave idle iroh streams open on the
   peer connection). Responses stream as they arrive. The caller's
   `Authorization` is dropped: the CLI authenticates. A server that cannot be
   reached, or a name that lists none, is an RFC 9457 problem, the shape the
   API's own errors have.
5. **`--token` reaches only the server it was given for.** The primary named
   with `--server` keeps it; every other server is reached as `forServer`
   reaches it, without it.

## Alternatives rejected

- **The program dials servers itself.** Every program would reimplement
  server resolution, the registry, the primary rule and an iroh client — and
  hold this machine's iroh identity, which is the CLI's to hold.
- **The program reads `servers.json` and dials only HTTPS servers.** It would
  miss every iroh and socket server, which are most of them, and drift from
  the CLI's rules as they change.
- **A daemon of its own.** A second long-running process with its own
  lifecycle and config, for what one CLI command does.
- **Loopback TCP by default, with a token in a file.** Any user on the
  machine can reach a TCP port; a token file is the socket's mode with more
  steps and more ways to leak.

## Consequences

- A local program needs only an HTTP client that can dial a unix socket, and
  the API's own OpenAPI document, to talk to every server the person has.
- The command is a long-running CLI process: it holds the iroh endpoint and
  its connections, and a program depending on it has to start it or ask the
  person to.
- Nothing new is exposed beyond what the user's own CLI can already do.
