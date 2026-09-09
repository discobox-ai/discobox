# 0094 — An image declares services in the format a repository does

- **Status**: Accepted
- **Date**: 2026-09-05
- **Relates to**: [ADR 0046](0046-listening-ports-are-polled-and-probed-in-the-background.md),
  whose probe this makes skippable;
  [ADR 0070](0070-services-are-declared-execs-the-sandbox-starts-for-you.md),
  whose declaration format this reuses and extends;
  [ADR 0076](0076-a-service-may-declare-a-port-discovery-cannot-see.md),
  which added the first declared-port input; and
  [ADR 0080](0080-a-skill-ships-with-the-interface-it-documents.md),
  whose image/repository directory pairing this copies.

## Context

ADR 0046 discovers a sandbox's ports by reading `/proc/net/tcp{,6}` filtered by
the run user's uid, then **probing each new socket once** to classify it. ADR
0076 added a second input for the sockets that filter cannot see — root holds
them — and those declared ports are probed the same way.

The desktop viewer breaks both halves.

**It cannot be discovered.** `discobox-desktop.socket` is bound by pid 1, so the
uid filter excludes it by construction. That is ADR 0076's case exactly.

**It must not be probed.** The desktop is socket-activated on purpose: nothing
runs until a browser asks for the page, and connecting to 6900 is what starts
the X server, the Xfce session, the VNC server and the viewer. A classification
probe is indistinguishable from a person opening the desktop, so the mechanism
that would tell us the port speaks HTTP is the mechanism that boots the whole
desktop — on a timer, in every sandbox, whether or not anybody wants one.

Probing is the only way to *learn* a protocol. It is not the only way to *know*
one. Whoever declares the port usually knows what it speaks, and for the desktop
the image knew at build time.

So something in the image has to declare a port and its protocol. The question
is what.

## Decision

**The image declares services in `/usr/local/share/discobox/services`, in the
same format and read by the same code as a repository's `.discobox/services`.**

```sh
#---
# name: Desktop
# port: 6900
# protocol: http
# start: never
#---
```

1. `services.Discover` reads both directories: the image's first, then the
   repository's. A repository declaration wins on a shared id, exactly as its
   skills win on a shared name (ADR 0080).

2. **`protocol:` is used instead of a probe, not alongside one.** The port is
   reported as speaking it and never connected to. A declaration that states no
   protocol is probed, which is ADR 0076 unchanged. The field is available to
   repository declarations too — an author writing `ports: 8080` knows what it
   serves.

3. **`start: never`** declares ports without declaring anything to run.
   Something else already serves them: a systemd socket unit, a nested
   container. It is not a broken script — a declaration with nothing to run has
   no shebang and no executable bit, and both are Problems only for a script the
   sandbox is meant to launch.

4. **`id:`** states the declaration's identity instead of deriving it from the
   filename, in lowercase reverse-DNS. It reaches the port snapshot as
   `serviceId`, alongside the display name as `serviceName` — the pair the exec
   metadata already carries, for the reason it carries it (ADR 0070 §7): the id
   is what a client matches on, and the name is what it labels the result with.

   A filename-derived id is enough for a repository's own services, where the
   file and the name move together. It is not enough for a service a client has
   to *recognize*. `ai.discobox.desktop` is the desktop viewer whatever the file
   is called, so a client gives it its own affordance — a Desktop link in the
   chrome — rather than listing it as an HTTP port on 6900.

   The `ai.discobox.` prefix is reserved for image declarations. A repository
   using it is listed with a problem rather than honoured: repository
   declarations win on a shared id, so one that claimed the desktop's would
   replace it and take its link with it.

This mirrors ADR 0080's pairing exactly: some things are true of the sandbox
whatever repository is being worked on in it, and cannot come from the
repository being worked on.

## Consequences

- One declaration format, one parser, one vocabulary. An image service is a
  service, listed by `discobox admin services` like any other.
- A client identifies the desktop by a string that is part of the contract
  rather than by a port number, so nothing outside the sandbox hardcodes 6900.
- An image service can also *run* something, since it is a real declaration and
  not a manifest entry — a capability nothing needs yet.
- A declared protocol is a claim the sandbox does not verify. An image that says
  `http` about a port serving something else is wrong in its file. Same trade
  ADR 0076 already made about the port number.
- The desktop stays cold until somebody opens it. Before this, the port watcher
  started it within one tick of every sandbox boot.
- **The control plane cannot know a harness has a desktop until a sandbox has
  booted.** Image labels are resolved at registration without pulling filesystem
  layers, and a directory inside the image is not a label. A harness list
  therefore cannot show "this one has a desktop" before one runs. Accepted: the
  link is only useful against a running sandbox anyway.

## Alternatives rejected

- **Declare services in the image manifest label.** This was built first, and
  rejected on review. It is the only option that gives registration-time
  visibility, and the cost is four hops of plumbing — a `harness.Service` type,
  a persisted column on `HarnessConfig`, two OpenAPI schemas, a pool-agent
  conversion and a `sandboxconfig` layer field — to carry data the sandbox could
  simply read off its own filesystem. It also creates a second, adjacent notion
  of "service" that means something narrower than the existing one, and cannot
  express a service that runs.
- **Probe it anyway, and accept the activation.** Rejected: it makes the
  laziness the desktop is built on unobservable in practice.
- **A probe that does not connect.** Rejected: systemd activates on the accept,
  so there is no connection that classifies a socket without starting what is
  behind it.
- **A well-known port table in the control plane.** Rejected: it puts image
  knowledge in the one component that must work with images it has never seen,
  and still says nothing about anything else an image serves.
