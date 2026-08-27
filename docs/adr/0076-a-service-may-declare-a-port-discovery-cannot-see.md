# 0072 — A service may declare a port discovery cannot see

- **Status**: Accepted (amends [0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md) §1)
- **Date**: 2026-08-25
- **Relates to**: [ADR 0046](0046-listening-ports-are-polled-and-probed-in-the-background.md),
  whose watcher this adds a second input to, and
  [ADR 0049](0049-forwarded-ports-are-bound-near-their-number-and-held.md),
  which is what a declared port then gets forwarded by.

## Context

[ADR 0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md) §1
dropped discobot's `port:` header from the service declaration, on the grounds
that ADR 0046's watcher answers it for free: *"a service that listens is already
reported and already forwarded by the workspace without declaring anything."*

That is true of a service that listens *as the sandbox user*, which is the only
thing ADR 0046 can discover. Discovery is `/proc/net/tcp{,6}` filtered by the
uid `execs.Manager.ResolveUser` returns, and that filter is load-bearing: it is
what makes "a port belongs to the sandbox's own work" true by construction
rather than by a guess about which ports are interesting. It also means a socket
the sandbox's work is plainly responsible for is invisible whenever some other
uid holds it:

- **Nested Docker.** `docker compose up` publishes `0.0.0.0:8080`, and the
  socket is held by `dockerd` (or a `docker-proxy` child) running as root. The
  user's `compose.yaml` put it there and the user's browser wants it, but no
  socket in the table is owned by the sandbox user.
- **Socket activation.** A `.socket` unit's listener is bound by pid 1, as root,
  before the service behind it has started at all — that is the whole point.
  The unit the user started owns nothing the filter can see.

Widening the filter is not an option: dropping the uid test reports every
listener in the sandbox, which in a systemd image is sshd, systemd-resolved, the
agent's own dependencies, and whatever else the image runs. The filter is the
only thing standing between a port listing and a port scan, and the failure it
prevents (a forward offered onto a system daemon) is worse than the one it
causes.

Nothing observable distinguishes these cases either. From outside the process
tree there is no signal that root's `:8080` is "the user's" and root's `:22` is
not. The missing fact is not observable at all — it is intent, and the only
place intent lives is a declaration.

## Decision

**A service declaration may name the ports it serves, and a declared port is
reported — and so forwarded — whatever procfs shows.** ADR 0070 §1's vocabulary
grows from `name`/`description` to `name`/`description`/`ports`:

```bash
#!/bin/bash
#---
# name: Docker Stack
# description: The API and its database, under compose
# ports: 8080, 5432
#---
exec docker compose up
```

### 1. Declaration is the second input to the watcher, not a second port list

`ports.Watcher` gains one seam — a function returning the declared set — and
folds it into the same tick, the same state map, and the same snapshot as the
procfs scan. It is not a parallel listing assembled by a client, and the status
payload does not grow a second array.

The alternative was to carry declared ports on the *service* record and let each
consumer union the two. Rejected: every consumer of ports (the CLI's `proxy`,
the workspace's automatic forward, the sandbox listing's port column) would have
to learn about services and do the union itself, and a consumer that forgot
would silently be the one place a declared port does not work. One listing that
is already the answer to "what can I forward" stays the answer.

The seam is a function called on every tick rather than a set passed at
construction, because ADR 0070 §5 re-reads declarations from disk on every
listing: a service file added or edited while the sandbox is up takes effect
immediately, and its port has to behave the same way. It is also why the watcher
does not import the service layer's discovery directly — `ports` is a procfs
watcher and knows nothing about repositories, the way it already knows nothing
about how a socket is probed.

### 2. A declared port is listed whenever it is declared, not while its service runs

The obvious refinement — list the port only while the declaring service's exec
is running — is wrong for the case that motivates this. A script that runs
`systemctl start app.socket`, or `docker compose up -d`, has *exited* by the
time the port matters; gating on the exec would drop the port precisely where
discovery already failed. And ADR 0070 §4 does not supervise services, so
"running" is not a state anything converges on.

So the declaration alone is the statement. The cost is accepted: a declared port
whose service is stopped is still listed and still binds a local port that
refuses connections. That is the same shape as ADR 0049's held binding for a
dev server that restarted, and it is the honest answer — the sandbox genuinely
does not know whether that port is up.

### 3. A declared port is still probed, and it is not called observed

Classification is unchanged: a declared port is probed exactly like a discovered
one, so a compose-published HTTP port reads `http` and gets a link in the
workspace. Probing works where discovery does not — connecting to `127.0.0.1:8080`
does not care which uid owns the far end — which is the asymmetry that makes
this a discovery problem and not a reachability one.

What the record must not do is claim more than it knows. `addresses` stays what
it has always been: the binds the scan actually saw, absent for a declared port
nothing visible is listening on. A new `declared` boolean says why the port is
on the record. Together they keep "the sandbox user is listening on 5432" from
being asserted when nobody is, which matters because the status payload is the
control plane's record of the sandbox and not just a hint for a picker.

Because a declared port has no socket of its own, it has no inode to key the
probe cache against, so its classification is cached against the declaration
instead. Consequence: a declared port that answered once keeps that answer for
the life of the declaration. Re-probing it on a schedule is exactly what
ADR 0046 rejected as a permanent low-rate scan of the user's own services.

### 4. Ports, plural, and the numbers are the whole vocabulary

`ports: 8080, 5432` — a YAML list, a comma-separated scalar, or a single number
all read the same. A compose stack is one service with several ports, and making
it two declaration files to say so would be a worse lie than the one this fixes.

Nothing else comes back with it. Discobot's header also carried `http` and
`path`; ADR 0070 dropped both and they stay dropped. `http` is what the probe
establishes, from the port itself rather than from a claim about it that can go
stale in the file, and `path` describes a URL to open, which nothing in this
system does yet.

## Alternatives rejected

**Declare ports on the sandbox or the harness image instead.** The manifest
already carries per-sandbox intent, and an image could declare the ports its
harness serves. Rejected for ADR 0070 §1's reason, unchanged: what runs beside
your work is a property of the thing being worked on, and a port that appears
because of a file you cannot find in the checkout is the failure that decision
already refuses. The declaration and the `compose.yaml` that publishes the port
should be the same commit.

**Report every listener and mark the ones the sandbox user owns.** This makes
the root-bound cases visible without any declaration, and lets a client decide.
Rejected: it turns the status payload into a full port scan of the sandbox, sent
to the control plane every 15 seconds and shown next to every sandbox in a
listing. The uid filter is a privacy and noise boundary, not an implementation
shortcut, and "the client will filter it" means every client re-derives a rule
that has one right answer.

**Find the port's owner instead of declaring it** — walk `/proc/*/fd` for the
socket inode, and count a root-held socket as the user's when it belongs to a
`dockerd` the sandbox user's `docker` client started, or to a unit that names
the user. Rejected as a heuristic with no bottom: it is a scan of every
process's descriptors per tick (ADR 0046 deferred that on cost alone), it does
not work at all for socket activation — pid 1 holds the socket and pid 1 belongs
to nobody in particular — and the cases it would have to special-case are open
ended. A declaration is one line in a file the user already writes.

**A `discobox ports add` runtime command.** Ad-hoc, no file to edit, works for
a port nobody anticipated. Rejected as the wrong first move rather than as a bad
idea: it is per-sandbox state to persist and reconcile, it does not survive
recreating the sandbox, and it does not travel with the branch. The declaration
gets the case that repeats; a runtime override is a separate decision.

## Consequences

- ADR 0070 §1's "`name` and `description` are the whole vocabulary" no longer
  holds; the declaration's schema is `name`, `description`, `ports`. That
  ADR's reasoning for dropping `port` stands for the case it was reasoning
  about — a service that listens as the sandbox user still declares nothing.
- `ports.Watcher` is no longer purely an observation of procfs. Its snapshot is
  now "the ports this sandbox serves" — what was observed, plus what was
  declared — and `declared`/`addresses` are what tell the two apart.
- The status payload gains a field (`declared`) and the service record gains one
  (`ports`), so an older control plane relaying a newer sandbox-agent's status
  carries a field it does not know. That is already true of the relay: the
  report body treats the payload as opaque.
- A malformed `ports:` value makes the declaration unrunnable, reported the way
  a missing shebang is. A port list that cannot be read is a declaration whose
  author meant something specific, and quietly running the script without its
  ports would be the invisible failure ADR 0070 §1 refuses.
- A declared port is probed on `127.0.0.1` — it has no observed bind to derive
  a target from. A service that binds only `::1` is still forwarded (the forward
  dials `localhost`, which tries both families) but classifies as `unknown`.

## Deferred

- **Attributing a listed port to the service that declared it**, so a port with
  nothing listening can say which declaration put it there. Revisit when a
  surface actually draws the reason — the same condition ADR 0046 deferred
  per-port process identity under, and the two want the same column.
- **A runtime port override**, per the rejected alternative. Revisit if a port
  that cannot be known at commit time — chosen by the tool, allocated per
  sandbox — turns out to be common.
