# 26-11-11-176 — An ephemeral port is listed but not probed, and a port is probed once it has stayed

- **Status**: Proposed (would narrow [0046](0046-listening-ports-are-polled-and-probed-in-the-background.md)
  §2, which probes every newly observed TCP socket at once, and
  [0076](0076-a-service-may-declare-a-port-discovery-cannot-see.md) §3 only
  in when an undeclared port is probed)
- **Date**: 2026-11-11
- **Relates to**: [0109](0109-a-bound-udp-port-is-listed-and-forwarded-as-datagrams.md)
  §1, whose ephemeral range this reuses;
  [26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md)
  §2, whose sign-in callback forward must keep working;
  [26-09-05-409](26-09-05-409-an-image-declares-services-in-the-format-a-repository-does.md)
  §2, the stated protocol that already skips the probe.

## Context

The sandbox agent probes every TCP socket its user starts listening on: it
connects and sends `HEAD /` (`User-Agent: discobox-sandbox-agent (port
probe)`), and again inside TLS when the answer leaves TLS open
(`sandbox-agent/ports/probe.go`, ADR 0046 §2). The probe is queued the tick
the socket is first seen (`Watcher.observe`, `sandbox-agent/ports/ports.go`),
which on Linux is within half a second of the bind (ADR 26-11-03-815 §1).

Test servers are listening sockets too. Go's `httptest.NewServer` and
`NewTLSServer`, and their equivalents in other languages, bind port 0, so
the kernel gives them a port from its ephemeral range (32768–60999 by default
on Linux). The probe reaches them while the test runs, and a test that
asserts exactly which requests its fake server got sees one it did not send.
A user's Go repository had at least ten tests in four packages failing in 3%
to 60% of runs, some hanging until the test timeout when the probe filled a
buffered channel. None of it happens in their CI. Rejecting the agent's
loopback connections to the ephemeral range with iptables took those tests to
zero failures in 50–100 runs each (#998). This repository has been patching
its own tests one at a time to ignore the probe (#605, and the open flakes
#975 and #969).

The obvious fix is the one ADR 0109 §1 already makes for UDP: drop ports in
the ephemeral range at discovery. For TCP that breaks a sign-in. GitHub
Copilot CLI's browser sign-in listens on `server.listen(0, "127.0.0.1")`, a
new ephemeral port each run, with no setting to fix it (measured for ADR
26-10-09-867). ADR 26-11-02-152 §2 relies on the workspace forward finding
that port: the console forwards each listed port at its own number when the
number is free, and that is where the browser's redirect lands. A port
dropped from the listing is never forwarded, and the sign-in fails.

But the forward does not need the probe. It is driven by the listing
(`cli/internal/portforward`): it binds the local end for each listed port and
dials into the sandbox only when a local client connects. The protocol only
labels the port and decides whether the console header makes it a link
(`portScheme`, `cli/internal/tui/list.go`). The link rewriter that points a
pane's printed URLs at the local end looks at bound TCP ports, not at the
protocol (`forwardedURL`, `cli/internal/tui/links.go`). Listing a port and
probing it are separable, and only the probe hits test servers.

## Decision

### 1. A TCP port in the ephemeral range is listed and forwarded, but not probed

Discovery is unchanged for TCP: every listening socket the sandbox user owns
is listed, in the ephemeral range or out of it. The probe skips an undeclared
TCP port whose number is inside the kernel's ephemeral range. The range is
the one discovery already reads for UDP (ADR 0109 §1): per scan, from the
sandbox's own network namespace (`/proc/sys/net/ipv4/ip_local_port_range` on
Linux, `net.inet.ip.portrange.first`/`last` on darwin), with the platform
default when it cannot be read. A sandbox user who changes the sysctl moves
the line on the next scan.

A port is bound inside that range almost always because something asked for
port 0. Test servers do that, and so does a sign-in callback. Dev servers
almost never do: they take a fixed number a person types or a framework
defaults to (3000, 5173, 8080), and are probed as before.

Such a port is reported as `unknown`, the value a port has before its probe
answers. It is not reported as `tcp`: that means "reached, and not HTTP",
which is not known, and is wrong for a callback that is HTTP. No new value is
added either: `protocol` is a closed enum in the API, and an older CLI's
generated decoder rejects a value it does not know. The API's description of
`unknown` changes from "retried" to also cover "not probed": a port in the
ephemeral range stays `unknown` until its declaration (§3) says otherwise.

The console draws it as `?`, forwarded at its own number when free, without a
header link. A URL the server prints in a pane is still pointed at the
forward.

### 2. Any other port is probed once it has stayed for three seconds

An undeclared TCP port outside the ephemeral range is probed on the first
scan at which the same socket has been listening for at least three seconds,
not on the scan that first sees it. Until then it is listed as `unknown`,
which it already is between the first scan and the probe's answer.

This covers a test server that binds a fixed number (a suite that calls
`app.listen(3001)`) and one in a sandbox whose ephemeral range was narrowed.
Such servers usually live for milliseconds to a second. A dev server lives
for minutes to hours, so the delay costs it three seconds before its link
appears, during which it is usually still starting.

The clock is the socket's, keyed the way the probe cache already is: a server
that restarts on the same port gets a new socket and waits again. On darwin,
whose scan runs every five seconds, this means the second scan that sees the
socket.

### 3. A declaration decides, both ways

A port a service declares (ADR 0076) is outside both rules. A declaration
that states a protocol is believed and never probed (ADR 26-09-05-409 §2). A
declaration that states none is probed at once, whatever its number. These
are the escape hatches, and they exist already:

- A real server on an ephemeral port that should be classified: declare its
  port in `.discobox/services`.
- A long-lived server on a fixed port that must not be touched: declare it
  with a `protocol:`, and `start: never` when the declaration should run
  nothing.

No setting is added.

## Alternatives rejected

- **Skip TCP ports in the ephemeral range at discovery** (issue option 1 as
  written). It stops the probe, but it also removes the port from the
  listing, so the workspace forward never binds it, and Copilot's browser
  sign-in, which ADR 26-11-02-152 §2 makes the default path, redirects to a
  port nothing forwards. It is right for UDP (ADR 0109 §1), where a port in
  the range is almost always a client's and the listing has no such
  consumer. For TCP the consumer exists.
- **Probe without sending a request** (issue option 2). Connect, read what
  the server says first, and send nothing. An HTTP server says nothing until
  it gets a request, and so do Postgres and Redis, so silence cannot tell
  HTTP from anything else. The probe would stop classifying the case it
  exists for. Sending a TLS ClientHello to tell HTTPS apart is itself traffic
  a test server sees: Go's TLS server logs a handshake error into the test's
  output for a connection that closes after it. Even a bare connection is
  visible to a test that counts connections or tracks connection state.
- **Only wait before probing** (issue option 3 alone). It covers a test that
  is over before the wait ends, but not an integration suite whose server
  lives for its whole run, often longer than any wait a dev server would
  tolerate. Such servers bind port 0, which §1 covers by number however long
  they live. The wait is kept for the fixed-number case §1 cannot see.
- **A per-repository or per-sandbox opt-out** (issue option 4), such as a
  `.discobox/settings.json` flag that turns the probe off. With §1 and §2,
  what is left is a long-lived server on a fixed port, and a declaration
  (§3) already keeps the probe off one port without turning classification
  off for every dev server in the repository. Deferred: revisit if a report
  shows a repository whose fixed-port servers cannot reasonably be declared
  one by one.
- **Recognize the process behind the port** (a `*.test` binary, `pytest`,
  `jest`) and skip it. It needs the `/proc/*/fd` walk ADR 0046 deferred for
  its cost, and a list of test runners per language that is never complete.
  The number the kernel picked says the same thing for free.
- **Probe only when someone opens the port.** It would remove the probe from
  the background entirely, but a client then learns what a port speaks only
  after acting on it, which is the problem ADR 0046 exists to solve. It
  changes the shape of the status payload, not just when the probe runs.
- **Report a new `unprobed` protocol value.** It is more precise than
  `unknown`, but the enum is closed and an older CLI fails to decode a
  sandbox that reports it. What a client does with `unknown` (no link, `?`)
  is what it should do here.

## Consequences

- A test server on a port the kernel picked gets no connection from the
  agent, however long it lives. One on a fixed port gets none unless it
  outlives the three-second wait.
- Copilot's callback, and any other port-0 listener, is still listed and
  forwarded at its own number. Its label is `?` rather than `http`, which no
  sign-in depends on.
- A real dev server on an ephemeral port loses its header link and its
  `http`/`https` label until it is declared. Its printed URLs still point at
  the forward.
- Every other dev server is classified about three seconds later than today.
  Nothing waits on the classification: the port is listed and forwarded from
  the first scan, as now.
- This repository's tests that ignore the probe (#605 and its follow-ups) can
  stay, and are no longer needed. #975 and #969 are re-checked against the
  change.

## What it needs

- `sandbox-agent/ports`: the watcher learns the ephemeral range (the
  scanners already read it for UDP) and a first-seen time per socket, and
  queues a probe by §§1–3 instead of on first sight. The API description of
  `protocol` and the package's `DESIGN.md` say what `unknown` now covers.
- A test that an undeclared listener in the ephemeral range is never probed
  and still appears in the snapshot, that a fixed-port listener is probed
  only after the wait, and that a declared one is probed at once.
