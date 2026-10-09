# 26-10-09-867 — A configure port may be ephemeral, and is still bound at its own number

- **Status**: Accepted
- **Date**: 2026-10-09

## Context

A harness's browser sign-in sends the user's browser back to a loopback
callback, `http://127.0.0.1:<port>/callback`. The configure sandbox runs that
listener; the browser runs on the user's machine. The image declares the port
in `config.ports` (`harness.ConfigPort`), and `discobox admin harnesses
configure` binds it locally at exactly that number (`portforward.Options.Exact`)
and forwards it in. A forward moved to the next free port would answer
nothing, because the redirect URI names one number.

That works for a CLI with a fixed port (Codex 1455, OpenCode 1455/1456). It
does not work for GitHub Copilot CLI 1.0.95. Measured: four browser sign-ins
redirected to 36133, 43625, 37287 and 33989; its bundle calls
`server.listen(0, "127.0.0.1")`; and no Copilot setting fixes the port
(`auth.redirectPort` and `oauth.callbackPort` apply to MCP servers' OAuth
only). Declaring a number, as was first tried with 41039, forwards a port
nothing listens on.

The workspace view already forwards whatever a sandbox is found listening on
(`tuiForward.follow`, fed by the sandbox-agent port watcher), but it moves a
taken port to the nearest free one.

## Decision

### 1. `config.ports` may hold one ephemeral entry

`{"ephemeral": true, "unavailable": "..."}`. It has no port number, and an
image declares at most one. The server rejects an ephemeral entry that names a
port, and a second ephemeral entry. Numbered entries keep their rules.

### 2. An ephemeral entry forwards each discovered TCP port at its own number

While the configure flow runs, the CLI polls the sandbox's reported listening
ports and forwards each discovered TCP port at the same number on the user's
machine, using the same exact bind as a declared port. A port taken locally is
never moved. When that happens, the CLI names the number and the image's
`unavailable` text follows, saying what to do. Only TCP is followed, because a
callback is an HTTP request. The image's declared services (the desktop on
6900) are in the same listing and are not followed, because no sign-in
redirects to them. The forward ends with the configure flow.

### 3. `port` stays a required integer on the wire

In the API, `HarnessConfigPort.port` stays required, and an ephemeral entry
sends `0` beside `ephemeral: true`. A CLI older than the server is not
supported here: its generated decoder rejects the unknown `ephemeral` field,
as it rejects any field added to a response. The TUI's in-use probe skips
ephemeral entries.

### 4. Copilot's image declares an ephemeral port

`harness/copilot/image.json` uses `{"ephemeral": true, ...}`. Its `unavailable`
text says the browser sign-in needs its callback port free, and if it is not,
to run `/login` again for another port or pick "Sign in with a device code".

## Alternatives rejected

- **`port: "ephemeral"`, a number-or-string field.** It reads naturally in the
  manifest, but the type of `port` would change for every client. A separate
  boolean keeps `port` an integer.
- **Declaring a guessed number (41039).** Copilot picks a new port on every
  run, so a guessed number is wrong almost every time.
- **Reusing the workspace forward as it is.** Its nearest-free search would
  bind a taken callback port somewhere the browser is never sent, and report
  it as forwarded.
- **Leaving it to device code.** That works, but browser sign-in is Copilot's
  recommended path, and the workspace already discovers ports.

## Consequences

- Latency: the CLI reads the control plane's copy of the sandbox's ports, which
  the pool refreshes every 15 s after the sandbox's own 5 s scan; with the CLI's
  2 s poll, a callback port can be bound up to about 22 s after it starts
  listening. A user who authorizes faster gets a browser that cannot connect,
  so the harness's instructions say to reload that page: the redirect carries
  the same code and state to the same port, which answers once it is bound. A
  live port path from the sandbox would close the gap and is not built here;
  revisit if reloading proves a real cost.
- A bind failure for a discovered port is reported mid-flow. A full-screen CLI
  may paint over it, as it can for a fixed port's warning.
- Every discovered TCP port the configure sandbox listens on is forwarded, not
  only the callback. Apart from its declared services, which are skipped, the
  configure sandbox runs only the harness's sign-in, and binds are loopback
  only.
