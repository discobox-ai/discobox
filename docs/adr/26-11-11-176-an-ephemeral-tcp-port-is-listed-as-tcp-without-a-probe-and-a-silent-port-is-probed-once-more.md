# 26-11-11-176 — An ephemeral TCP port is listed as `tcp` without a probe, and a silent port is probed once more

- **Status**: Proposed (would narrow [0046](0046-listening-ports-are-polled-and-probed-in-the-background.md)
  §2, which probes every newly observed TCP socket exactly once and calls a
  port `tcp` only once it was reached, and its Consequences, under which a
  non-HTTP service is written to once per socket; and
  [0076](0076-a-service-may-declare-a-port-discovery-cannot-see.md) §3,
  under which a declared port is probed like a discovered one and keeps its
  first answer for the life of the declaration)
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
its own tests one at a time to ignore the probe: #605, and the flakes #975
and #969, closed by test-side patches that leave the cause in place.

The obvious fix is the one ADR 0109 §1 already makes for UDP: drop ports in
the ephemeral range at discovery. For TCP that breaks a sign-in. GitHub
Copilot CLI's browser sign-in listens on `server.listen(0, "127.0.0.1")`, a
new ephemeral port each run, with no setting to fix it (measured for ADR
26-10-09-867). ADR 26-11-02-152 §2 relies on the workspace forward finding
that port: the console forwards each listed port at its own number when the
number is free, and that is where the browser's redirect lands. A port
dropped from the listing is never forwarded, and the sign-in fails. Copilot
also gives the browser little time: the port has to be listed, and
forwarded, as soon as it is bound.

But the forward does not need the probe. It is driven by the listing: every
listed port that is not `udp` is forwarded as a TCP byte stream, whatever its
protocol says (`portNetwork` and `sandboxPortTargets`,
`cli/internal/cli/listening_ports.go`), and the forward dials into the
sandbox only when a local client connects (`cli/internal/portforward`). The
protocol only labels the port and decides whether the console header makes
it a link (`portScheme`, `cli/internal/tui/list.go`). The link rewriter that
points a pane's printed URLs at the local end looks at bound TCP ports, not at
the protocol (`forwardedURL`, `cli/internal/tui/links.go`). And the agent
publishes a new port in the scan that finds it, before any probe runs
(`Watcher.tick`). Listing a port and probing it are separable, and only the
probe hits test servers.

The probe also has a race of its own. A server binds and listens before its
accept loop or its handlers are ready, so the kernel accepts the probe's
connection and nobody answers it within the probe's one-second read. That
reads as a hang-up without a word, the TLS attempt fails the same way, and
the port is classified `tcp` for the life of its socket: an HTTP dev server
with no link until it restarts.

## Decision

### 1. A TCP port in the ephemeral range is listed as `tcp`, and never probed

Discovery is unchanged for TCP: every listening socket the sandbox user owns
is listed, in the ephemeral range or out of it. An undeclared TCP port whose
number is inside the kernel's ephemeral range is reported as `tcp` from the
scan that finds it, and is never connected to, the way a UDP port is `udp`
from the table it was found in (ADR 0109 §2). It is listed, and so forwarded,
exactly as fast as today.

`tcp` is what is known: the port is a TCP listener. Nothing is claimed about
what it speaks, and no client needs more. `http` and `https` are what earn a
header link, and an unprobed port has none, which is what it would also have
as `unknown`. `unknown` keeps its one meaning, not classified yet and retried.
No new value is added: `protocol` is a closed enum in the API, and an older
CLI's generated decoder rejects a value it does not know. The API's
description of `tcp` grows to cover a port in the ephemeral range that was
never probed.

The range is the one discovery already reads for UDP (ADR 0109 §1): per scan,
from the sandbox's own network namespace (`/proc/sys/net/ipv4/ip_local_port_range`
on Linux, `net.inet.ip.portrange.first`/`last` on darwin), with the platform
default when it cannot be read. A sandbox user who changes the sysctl moves
the line on the next scan.

For this decision only, the range is never taken below the platform
default's low end (32768 on Linux, 49152 on darwin). UDP discovery keeps
using the range as read (ADR 0109 §1, unchanged). A range raised or narrowed
above the floor is used as read. A range widened below it, such as
`1024 65535` (a common tuning value), is cut off there. Otherwise every dev
server would fall inside the range and lose its classification without
anyone being told. A port below that line is probed like any other (§2), even
when the kernel did pick it.

A port is bound inside that range almost always because something asked for
port 0. Test servers do that, and so does a sign-in callback. Dev servers
almost never do: they take a fixed number a person types or a framework
defaults to (3000, 5173, 8080).

The skip is decided again on every scan, from the range and the declarations
as they are then. It is not a probe result, and is not cached like one. A
port that leaves the range because the sysctl was raised, or that a
declaration now names, is queued for its probe on the next scan, though its
socket has not changed.

### 2. Any other port is probed at once, and once more when it answered nothing

An undeclared TCP port outside the ephemeral range is probed on the scan
that first sees it, as today.

When that probe reaches the port and gets no bytes back at all, plaintext or
inside TLS, it classifies it `tcp`, as today, and the port is probed once
more, on the first scan at least three seconds later, if the same socket is
still there. The second answer stands for the socket's lifetime, whatever
it is. That is what closes the startup race: a server that took the first
connection before it could answer is answering by then. A silent answer is
the only shape the race takes. A port that answered with bytes that are not
HTTP (an SSH or MySQL banner, Redis's error reply) was running and is not asked
again. `http` and `https` are proof and are not repeated either. `unknown`, a probe that could not connect,
is retried on the next tick as today.

One repeat, not a schedule. Asking a port the same question every few seconds
for as long as it is not HTTP is the standing scan of a user's services ADR
0046 rejected. A server still not ready after three seconds keeps `tcp` until
it restarts, which is today's behaviour for every server.

On darwin, whose scan runs every five seconds, the repeat is the next scan.

### 3. A declaration decides, both ways

A port a service declares (ADR 0076) is outside §1. A declaration that
states a protocol is believed and never probed (ADR 26-09-05-409 §2). A
declaration that states none is probed by §2, whatever its number, repeat
included. Its probe and its repeat are keyed the way its probe cache already
is: by the socket when one is observed, and by the declaration when nothing
visible listens (ADR 0076 §3). These are
the escape hatches, and they exist already:

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
- **Report an unprobed ephemeral port as `unknown`** (this ADR's first draft).
  It forwards the same and draws no link either. But `unknown` means "not
  classified yet, retried", and a port that will never be probed is not
  waiting for anything. `tcp` is true of it, and keeps `unknown` meaning one
  thing.
- **Wait before probing every port** (issue option 3, and this ADR's first
  draft, with three seconds). It covers a test server on a fixed port that
  is over before the wait ends, but not one whose suite outlives the wait,
  and it costs every dev server its label and link for those seconds. Port-0
  test servers, the reported case, are covered by §1 however long they live.
  The three seconds are kept where they buy something: before the one
  repeat in §2.
- **Probe without sending a request** (issue option 2). Connect, read what
  the server says first, and send nothing. An HTTP server says nothing until
  it gets a request, and so do Postgres and Redis, so silence cannot tell
  HTTP from anything else. The probe would stop classifying the case it
  exists for. Sending a TLS ClientHello to tell HTTPS apart is itself traffic
  a test server sees: a handshake it never asked for, which Go's `net/http`
  reports as a TLS handshake error when the probe hangs up. Even a bare
  connection is visible to a test that counts connections or tracks
  connection state.
- **A per-repository or per-sandbox opt-out** (issue option 4), such as a
  `.discobox/settings.json` flag that turns the probe off. With §1, what is
  left is a server on a fixed port, and a declaration (§3) already keeps the
  probe off one port without turning classification off for every dev server
  in the repository. Deferred: revisit if a report shows a repository whose
  fixed-port test servers cannot reasonably be declared one by one.
- **Repeat the probe on any `tcp` answer**, silent or not. It would also
  catch an HTTP server that, while starting, answers something else first,
  which nothing observed does. It costs every service with a banner or an
  error reply a second malformed request on every socket.
- **Repeat the probe until the port answers as HTTP.** It would also catch a
  server slower than three seconds to start, but a database or an SSH daemon
  never answers as HTTP, and would be asked on a timer forever (ADR 0046).
- **Recognize the process behind the port** (a `*.test` binary, `pytest`,
  `jest`) and skip it. It needs the `/proc/*/fd` walk ADR 0046 deferred for
  its cost, and a list of test runners per language that is never complete.
  The number the kernel picked says the same thing for free.
- **Probe only when someone opens the port.** It would remove the probe from
  the background entirely, but a client then learns what a port speaks only
  after acting on it, which is the problem ADR 0046 exists to solve. It
  changes the shape of the status payload, not just when the probe runs.
- **Report a new `unprobed` protocol value.** The enum is closed, and an
  older CLI fails to decode a sandbox that reports it.

## Consequences

- A test server on a port the kernel picked gets no connection from the
  agent, however long it lives.
- A test server on a fixed port is probed as today, and twice when it sends
  nothing back within a second (a fake whose handler blocks) and is still
  listening three seconds later.
- Copilot's callback, and any other port-0 listener, is listed as `tcp` in
  the scan that finds it and forwarded at its own number, as fast as today.
  No probe runs before or after.
- A real dev server on an ephemeral port is `tcp`, with no header link,
  until it is declared. Its printed URLs still point at the forward.
- A dev server that was slow to answer its first probe gets its `http` or
  `https` label and link three seconds later instead of never.
- A non-HTTP service on a fixed port that says nothing to `HEAD /` within a
  second is written to twice per socket instead of once, and may log a
  second malformed request. Postgres is one: it reads the request as a
  startup packet, logs an error on its side and closes without a word. One that answers with any bytes is written to
  once, as today.
- A sandbox whose ephemeral range was widened below the platform default
  probes the ports below that default, so a test server the kernel put there
  is reached as today.
- This repository's tests that ignore the probe (#605, and the patches that
  closed #975 and #969) stay. They are still needed for a test server on a
  fixed port, but no longer for one on a port the kernel picked.

## What it needs

- `sandbox-agent/ports`: the watcher learns the ephemeral range (the
  scanners already read it for UDP) and reports an undeclared TCP port in it
  as `tcp` without queuing a probe. The range reaches the watcher through the
  `Scanner` seam, not a second read of the sysctl. The skip is re-decided on
  every scan (§1), kept apart from a probe's `tcp`. `Probe` reports whether
  its `tcp` was silent. Per probe-cache key, the watcher keeps when a silent
  `tcp` came back and whether the repeat has run, dropped with the key, and
  queues the repeat by §2.
- The texts that describe today's probe, updated in the same change:
  - the `protocol` description in both `api/openapi/server.yaml` and
    `api/openapi/sandbox.yaml`, whose `tcp` now also covers an unprobed port
    in the ephemeral range;
  - the `ports` row of `sandbox-agent/DESIGN.md`'s package map ("probes each
    newly seen socket once");
  - the port discovery note in `sandbox-agent/REVIEW.md` ("connections to
    every listening TCP socket");
  - the root `REVIEW.md` note on test servers that count requests, narrowed
    to a server on a fixed port (asked once, or twice when it sends nothing
    back within a second), not removed;
  - "What the user sees" in the in-box discobox skill
    (`sandbox-agent/features/core/skills/discobox/SKILL.md`), which already
    tells a UDP server to bind below the ephemeral range, and says the same
    for a TCP server that wants its link.
- Tests that an undeclared listener in the ephemeral range is `tcp` in the
  first snapshot and never probed; that a fixed-port listener is probed at
  once; that a silent `tcp` answer is probed once more after three seconds
  and not again, and a banner is not; that a replaced socket starts over;
  that a declared port in the ephemeral range is probed; and that a listed
  ephemeral port is probed once a declaration names it or the range moves
  past it.
