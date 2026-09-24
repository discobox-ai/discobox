# 0145 — A sandbox declares its platform, and a non-Linux one is a VM template

- **Status**: Accepted
- **Date**: 2026-09-23
- **Relates to**: [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md),
  [0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
  [0027](0027-harness-terminals-run-as-a-shells-typed-in-job.md),
  [0032](0032-every-sandbox-has-a-harness-config-and-shell-is-the-built-in.md),
  [0086](0086-a-harness-image-extends-the-base-and-its-manifest-is-override-only.md),
  [0099](0099-the-cli-downloads-the-server-it-starts.md),
  [0113](0113-the-cli-stages-the-images-its-server-loads.md),
  [0115](0115-exec-state-converges-on-notifications-not-a-poll.md),
  [0123](0123-a-discobox-is-exported-as-its-spec-and-its-durable-tree.md),
  [0126](0126-a-sandbox-does-not-share-a-host-or-a-filesystem-with-its-pool.md),
  and [0144](0144-a-pool-of-host-vm-sandboxes-runs-its-agent-on-the-host.md).
  0144 assumes the platform attribute this decision defines.

## Context

Every sandbox today is a Linux container built from an OCI image whose labels
carry its harness manifest (ADR 0086). The sandbox agent's PID-1 flow wires
declared volumes, systemd supervises every exec (ADR 0115), run identity is a
uid and a gid resolved from the image's passwd database (ADR 0025), and the
observations the status endpoint reports come from cgroups and `/proc`.

We want a sandbox that *is* a macOS or Windows machine, for the work that can
only happen there. Those are guest VMs on the user's own hardware, created and
reached by a pool agent running natively on it (ADR 0144). Nothing about them
is a container: there is no image to wire volumes from, no systemd, no passwd
database, no `/proc`, and on Windows no uid at all.

Two constraints shape what can be shipped. Neither vendor permits us to
redistribute their operating system, so Discobox cannot publish a macOS or
Windows image the way it publishes a harness image. And a guest that a pool
must automate has to hold the sandbox agent, which means the template is
something built rather than downloaded whole.

What we are not willing to trade is the API. A client attaches to a terminal,
lists execs, reads a screen, opens a tunnel and applies work the same way
whatever the sandbox is; a platform matrix in the CLI, the TUI and the control
plane would be a worse cost than any runtime difference it papered over.

## Decision

### 1. A sandbox's platform is declared, and it is a placement key

A sandbox's template declares the platform it runs — an OS and an architecture,
spelled the way the rest of the system already spells one. The control plane
records it on the sandbox, and a pool declares which platform it hosts. A
sandbox is placed only on a pool of its own platform, and a mismatch is refused
at placement with that as the reason.

A pool hosts exactly one platform. It follows from 0144 §1, and it is what
keeps the pool's own services honest: a pool with no BuildKit is not a degraded
Linux pool, it is a pool whose sandboxes have no builds to run.

### 2. A non-Linux template is assembled on the machine

The base operating system comes from the vendor, on the user's own machine:
Apple's restore image for a macOS guest, a Windows installation source the user
supplies. Discobox publishes the **overlay** — the sandbox-agent binaries, the
manifest, and the provisioning that installs them into a fresh guest — and the
pool assembles the two into a template it then clones per sandbox.

The overlay is not an image and is not published as one. It is a small set of
release assets — the agent binaries for that platform, the manifest document,
and the provisioning that installs them — described by `releasemanifest` with a
digest each, downloaded over HTTPS and verified on the way to disk, exactly as
a release CLI stages the server it starts (ADR 0099, `serverstage`). Nothing
VM-shaped travels: not the vendor's base, which we may not redistribute, and
not the assembled template, which never leaves the machine that built it.

A template's identity is therefore two things — the vendor base version and the
overlay's release and digests — and both belong to the pin. A pin has to name
what would change the sandbox underneath its user (ADR 0016), and on these
platforms the base moves for reasons the overlay knows nothing about.

Assembling a template is slow and happens once per machine per base version. It
is a pool operation with its own progress phase, not something a create hides:
a first macOS sandbox on a new machine waits for an OS install, and saying so
is better than a create that appears to hang (ADR 0060).

### 3. The manifest contract survives, and absence is declared

The harness manifest is unchanged in shape and in layering (ADR 0086 §2): the
same fields, the same merge by identity, the same reserved layer numbers, the
same `discobox-harness-run` convention a terminal types into a shell
(ADR 0027). It ships as one of the overlay's own files rather than as a
container image's labels, and the resolver reads the same layers from there.

What a platform does not have is **declared in that manifest, not discovered at
runtime**. A macOS template declares no volumes, no `additionalGroups`, and no
desktop; a Windows one declares its own paths and shell. The manifest is where
optionality already lives, which is why this does not become a set of runtime
capability probes or optional Go interfaces: code paths stay required and
implemented, and the document says what this sandbox is made of.

### 4. The API does not fork; three seams inside the agent do

Routes, DTOs, scopes, exec and terminal semantics, the attach stream and its
frames, resume, and the status payload are one contract across platforms. A
client cannot tell from the API which platform it is talking to, except by
reading the platform field and the paths.

Three things behind that contract get a per-OS implementation, each a required
seam with an implementation for every platform the product supports:

- **Supervision.** systemd transient units and D-Bus notifications (ADR 0115)
  are Linux's. Elsewhere the agent supervises its own shims and converges exec
  state from the runtime files they already write. The rule ADR 0115 states
  survives the change: the shim's own write is the sole accurate source of an
  exit status, and what the supervisor is authoritative for is whether the run
  is still there.
- **Run identity.** §5.
- **Observation.** Listening ports, resource counters, and the idle stop read
  `/proc` and cgroups today; each platform answers the same questions its own
  way. The data contract is what must not drift: cumulative counters and never
  a rate (ADR 0071), listening sockets owned by the sandbox's own user, and an
  idle clock moved by a changed screen rather than by output (ADR 0124).

The PTY is part of no seam on macOS, which has one; on Windows it is ConPTY,
which the CLI already reached for (ADR 0065). Where a platform cannot express
something the protocol carries — a Windows process group has no `SIGSTOP` — the
agent maps it to that platform's nearest real mechanism and says so in the
exec's record rather than silently doing nothing.

### 5. Where there are no POSIX ids, a sandbox has one account

`sandboxuser`'s uid and gid are Linux's, and nothing invents them elsewhere. A
non-Linux sandbox runs as the single account its template provisions, named in
the manifest, and an exec request may not name another user or another group
set. ADR 0141 has the guest give a created account its ids; on these platforms
there is no numeric id to give, so the template's own account is the answer and
the manifest carries its name alone. ADR 0025's contract stands where it has meaning — the sandbox resolves its
own identity, and no component outside it guesses one — and on these platforms
the resolution has exactly one answer.

That is a real limitation and it is chosen deliberately: a second account on
Windows means a logon token, which means credentials or a service privilege,
and nothing in the product needs it yet. A request naming a user on such a
sandbox is refused with that reason, never quietly ignored.

### 6. Paths and volumes belong to the platform

Path validation, the working root, source targets and `workingDir` are judged
by the sandbox's platform rather than as Linux paths on every host, which is
what `harness.ResolveVolumes` does today. The `%HOME%`, `%UID%` and `%GID%`
tokens survive where they have meaning, and `~` keeps being something only the
sandbox can resolve.

Declared volumes (ADR 0007) are a container mechanism: they exist to wire an
image's paths onto primary volumes a pool mounted, and a VM has neither. A
non-Linux sandbox's disk is its `data`, its cache is a directory on that disk,
and there is nothing to bind. This is the same conclusion ADR 0126 §2 reaches
from the other direction, where a sandbox's roots are private anyway.

### 7. Trust, egress, and what is simply absent

The pool's MITM CA is installed into the guest's own trust store at boot, and
the agent still names the bundle in `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`,
`REQUESTS_CA_BUNDLE` and `PIP_CERT`, because the runtimes that ignore a system
store ignore it on every platform. Egress stays proxy-only, which on these
platforms is the backend's obligation (ADR 0126 §6).

Absent, and declared absent: nested Docker and the runc wrapper that injects
trust into it (ADR 0020), the pool-shared build path (ADR 0044), the Xfce
desktop, and the nix and Homebrew seeds. Each is a Linux-container mechanism
whose subject does not exist here; none is a feature waiting to be ported.

### 8. The durable tree is content, and a transfer stays on its platform

An export carries what ADR 0123 says it carries — `data`, `sources`, and the
pool's `origins` — as file content and the metadata a POSIX archive holds.
Platform-specific metadata does not travel: Windows ACLs, macOS resource forks
and extended attributes are not reconstructed on the far side, and a restore
onto a different platform is refused rather than approximated. A discobox moves
between machines, not between operating systems.

How a stopped guest's disk is read is the backend's, per ADR 0126 §8.

## Alternatives rejected

- **Support macOS and Windows only as pool hosts, and keep every sandbox
  Linux.** It is where the product already is, and it gives nobody an Xcode
  build, a `codesign`, a Windows toolchain, or a native browser to test in.
  The reason to do this work is the environment, not the hardware.
- **Publish complete macOS and Windows images.** Neither licence permits
  redistributing the OS, and the shape would be wrong even if one did: a
  multi-gigabyte base on every release line, re-downloaded for a change to an
  agent binary. The overlay is the part that is ours and the part that moves.
- **Ship the overlay as an OCI artifact, the way the pool's guest image
  travels.** The pool guest does go through a registry today
  (`server/providers/guestimage`, ADR 0113 §5), and this deliberately does not
  extend that. A VM template has no relationship to OCI: there are no layers,
  no config, and no container to run, so a registry would be a blob store
  wearing a media type, bringing registry auth and a client to code paths that
  otherwise need neither. What is actually being moved here is a handful of
  files with digests, which is what the release asset path already carries.
- **A per-platform sandbox-agent API.** The API is the product's surface. A
  platform matrix would reach the CLI, the TUI, the control plane and every
  client anyone writes, to spare the agent three internal seams.
- **Advertise platform differences as runtime capabilities.** It is the
  optional-interface shape this repository rejects: code paths that may or may
  not be implemented, and callers that must ask. Declaring absence in the
  manifest puts the variation in a document, where the rest of a sandbox's
  variation already lives.
- **Drive the guest over SSH or the hypervisor's guest tools instead of running
  the agent inside it.** Execs, terminals, the attach stream with its resume
  and screen semantics, services, tools and status would each have to be
  rebuilt per platform on top of a command channel. The agent is what makes a
  sandbox a discobox; a VM without one is a VM.
- **Run Linux containers inside the macOS or Windows guest to keep the existing
  mechanisms.** It reintroduces the whole container stack to avoid three
  seams, and hands the user a Linux environment again — which is the thing they
  did not ask for.

## Consequences

- sandbox-agent becomes a program that is built, tested and released for
  darwin and windows as well as linux. The windows cross type-check already in
  CI is the floor, not the bar: these platforms need real test lanes.
- A harness for a non-Linux platform is an overlay built by different tooling
  than a Dockerfile, and the harness catalog gains a platform per entry, so a
  client offers only what a pool can run.
- A first create on a new machine includes acquiring a vendor base and
  assembling a template. That is minutes, sometimes interactive, and it must be
  visible as its own phase rather than charged to the sandbox's create.
- Apple's licence caps concurrent macOS guests per host, and Hyper-V lifecycle
  needs privilege (ADR 0144), so a non-Linux pool's capacity is bounded by
  things no amount of hardware changes.
- Declared services and tools keep their file shapes and are run by the
  platform's shell, so a declaration written for one platform is not portable
  to another. That is the same property a shell script has everywhere.
- A discobox cannot be transferred across platforms, which is a new refusal in
  a flow (ADR 0123) that otherwise moves anything anywhere.

## Deferred

- **Execs as a user other than the sandbox's own on Windows.** Revisited when
  something needs it and a token path exists that does not mean storing a
  credential.
- **A desktop for a non-Linux sandbox.** Both platforms have a native remote
  display; revisited when someone needs to watch a sandbox's screen rather than
  its terminals.
- **Version-pinning a harness agent inside a non-Linux sandbox** (ADR 0114's
  store is a shell script over an npm layout). Revisited when harness pinning
  matters on these platforms.
- **Cross-platform transfer.** §8 refuses it; revisited only if a portable
  representation of a tree's platform metadata turns out to be worth having.
