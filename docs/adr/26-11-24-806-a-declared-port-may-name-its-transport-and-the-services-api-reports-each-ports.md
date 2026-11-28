# 26-11-24-806 — A declared port may name its transport, and the services API reports each port's

- **Status**: Accepted
- **Date**: 2026-11-24
- Accepted by the owner by merging #1655, 2026-11-25
- **Issue**: #23
- **Supersedes in part**: [0109](0109-a-bound-udp-port-is-listed-and-forwarded-as-datagrams.md)
  §2's "a declaration names one transport for all its ports, so a service
  serving both takes a second declaration, with `start: never`, for its UDP
  side". The rest of 0109 stands.
- **Narrows**: [26-09-05-409](26-09-05-409-an-image-declares-services-in-the-format-a-repository-does.md)
  §2. `protocol:` becomes the declaration's default for the ports that do not
  name a transport, instead of a statement about every port it names.
- **Relates to**: [0076](0076-a-service-may-declare-a-port-discovery-cannot-see.md)
  (declared ports), [0118](0118-whoever-serves-an-api-is-strict-and-whoever-reads-it-is-tolerant.md)
  (strict readers, still deferred), and
  [26-10-11-657](26-10-11-657-a-stopped-discobox-is-read-from-what-it-last-posted.md)
  (the posted inventory a stopped box's services listing is answered from).

## Context

A service declaration (`.discobox/services/*`, or the image's directory) names
its ports in `ports:`, and one `protocol:` applies to all of them. ADR 0109 §2
added `protocol: udp`, which makes every port of the declaration a UDP port.
A service that serves the TCP and UDP port of one number therefore takes two
files: one that runs it and declares `53`, and one with `protocol: udp` and
`start: never` that declares `53` again and runs nothing. That pair, DNS on
`53/tcp` and `53/udp` (or QUIC beside HTTPS), is the case 0109 itself gives as
motivation. It is also the case declarations exist for: a DNS server published
by a nested `docker compose` holds root-owned sockets that discovery cannot
see.

The spelling people reach for, `ports: 53, 53/udp`, is the one
`discobox proxy --port` already accepts (`parseProxyPorts`,
`cli/internal/cli/proxy.go`). Today it fails the whole declaration with a
`Problem`, so the TCP port is lost too and the service does not run.

At `e6f5ea446`:

- `parsePorts` (`sandbox-agent/services/discover.go`) accepts bare integers
  only. `Definition.Ports` is `[]int`, and `Definition.Protocol` is one value for
  the file.
- `Manager.Declarations` (`sandbox-agent/services/manager.go`) already emits a
  `ports.Declaration` with a protocol per port, and already keys duplicates on
  (number, is UDP). The port watcher and everything downstream of it need no
  change.
- `SandboxService.ports` (`api/openapi/server.yaml`, derived into `sandbox.yaml`)
  is an `int64` array. `SandboxService` carries no protocol at all, so a client
  cannot tell a `protocol: udp` declaration's ports from TCP ones even today.
- Every generated reader is strict (`additionalProperties: false`, ADR 0118,
  deferred). A response carrying a field an older reader does not know fails
  that reader's whole decode. Changing a field's type fails it the same way.
- Only the CLI decodes `SandboxService` (`cli/internal/cli/sandbox_services.go`,
  the `discobox admin service` table's ports column). The control plane proxies
  a live answer as bytes, and keeps a box's posted inventory as the JSON the
  agent posted (`server/internal/resources/sandboxes/inventory.go`). A stopped
  box's services listing is answered from that inventory, in whatever shape the
  box's agent wrote it.

So both shapes of API change break an older CLI talking to a newer agent, and
that pairing is already accepted as "upgrade the CLI" (0118). The pairing that
has to keep working is the other one: a newer CLI reading an agent that
predates this, or the inventory a stopped box posted before it.

## Decision

### 1. A port may carry a `/udp` or `/tcp` suffix

Every spelling of `ports:` and `port:` (a scalar, a comma- or space-separated
list, a YAML list, in a script's front matter or a `.yaml` declaration) accepts
`N`, `N/tcp` and `N/udp`. The suffix is case-insensitive, as it is for
`discobox proxy --port`. Anything else after the `/` is a `Problem`, as a
non-number is today.

```sh
#---
# ports: 53, 53/udp
#---
```

### 2. `protocol:` is the default for ports with no suffix

A port's transport is its suffix. Without one, it is UDP when the declaration
says `protocol: udp`, and TCP otherwise. That keeps every declaration valid
today meaning what it means today.

A port's stated protocol, the one reported instead of probing it
(26-09-05-409 §2), follows from that:

| Port | `protocol:` | Stated protocol |
| --- | --- | --- |
| UDP, by suffix or default | anything | `udp` |
| TCP | `http`, `https` or `tcp` | that |
| TCP | `udp` or nothing | none: probed, as an undeclared protocol is |

The suffix says only the transport. `ports: 8080/tcp` with `protocol: http` is
an HTTP port, and `ports: 53/tcp` with `protocol: udp` is a TCP port that is
probed. The two do not conflict: `protocol: udp` with one `N/tcp` reads as
"these are UDP, except this one", and refusing it would make the author restate
every other port with a suffix to say something the file already says. A
declaration that wants a stated protocol on a TCP port as well as UDP ports
writes the TCP default and suffixes the UDP ones (`protocol: http`,
`ports: 8080, 8443/udp`).

A port is keyed by number and transport within a declaration, as across
declarations (0109 §1). `53` and `53/tcp` in one file are one port; `53` and
`53/udp` are two.

### 3. A definition's ports carry their transport

`services.Definition.Ports` becomes a list of ports that each carry their
transport, in the order the file names them. `Definition.Protocol` stays the
declaration's stated value, now read as §2's default. `Declarations` takes
each port's transport and stated protocol from the port, not from the file.
The watcher's input (`ports.Declaration`) is unchanged.

### 4. The services API reports each port with its protocol, in a new field

`SandboxService` gains `declaredPorts`, an array of `SandboxServicePort`
(`additionalProperties: false`):

- `port` (`int64`, required).
- `protocol` (`http`, `https`, `tcp` or `udp`): the port's stated protocol by
  §2, absent when none is stated. `udp` is UDP and everything else is TCP,
  the rule `SandboxAgentListeningPort.protocol` already follows (0109 §3).

It lists every declared port, in the order the file names them. `ports` stays,
with each declared number once in first-appearance order, and its description
says to read `declaredPorts`. The sandbox-agent always sends both.

A reader takes `declaredPorts` when present. When it is absent, the answer came
from an agent older than this, live or through its posted inventory, and the
reader falls back to `ports`, reading them as TCP. That is all such an answer
ever said.

The CLI's ports column writes TCP ports bare and UDP ports as `N/udp`, the
spelling a person would declare or forward them with: `53,53/udp`.

### 5. The two-file recipe still works and is no longer documented

Two declarations naming one number on two transports still give two ports, as
they do today. `sandbox-agent/DESIGN.md`'s services row and the in-box
`discobox` skill describe the suffix instead, and drop the recipe.

## Alternatives rejected

**Replace `ports` with an array of `{port, protocol}` objects.** The obvious
shape, and the one this would have been without history. Rejected because the
newer CLI is the reader that has to keep working, and a stopped box's posted
inventory is a persisted `ports: [53]` the control plane serves as written. A
CLI whose `ports` items are objects fails the whole services listing for that
box until it is started again with a newer agent. A stopped box can stay
stopped for a long time.

**A parallel `udpPorts: [int]`.** Smallest addition. Rejected because it
leaves `ports` ambiguous: with `ports: [53]` and `udpPorts: [53]`, nothing says
whether 53 is also TCP, unless `ports` changes meaning to "TCP only", which
misreads every older answer whose `ports` were a `protocol: udp`
declaration's. It also still leaves the stated protocol, which `SandboxService`
has never reported, unreported.

**Send `declaredPorts` only when it says something `ports` cannot.** That would
keep an older CLI working against a newer agent for every box without a UDP
declaration. Rejected because it makes one field present or absent depending
on content, so every reader carries two readings forever, to protect a pairing
0118 already accepts breaking. It would also lose the stated protocol for TCP
ports.

**Reject `protocol: udp` together with an `N/tcp` suffix.** Clearer on paper.
Rejected because the combination has one reading (§2), and refusing it fails
the whole declaration, including the service it runs, for a file whose meaning
is plain.

**A suffix that states a protocol (`8080/http`).** Rejected because the
suffix is the transport spelling `discobox proxy --port` and Docker publishes
already use, and one field meaning two things would let `53/http` be written.
`protocol:` already states protocols.

**Keep the two-file recipe as the answer.** Rejected as the issue's own
problem: the spelling people reach for fails, and it fails the TCP port with
it.

## Consequences

- A CLI older than this fails the services listing (`discobox admin service`)
  against an agent that has it, over the unknown `declaredPorts`. That is the
  breakage any `SandboxService` field causes under strict readers (0118), and
  the answer is the same: upgrade the CLI.
- A newer CLI reads an older agent's answers and stopped boxes' inventories
  unchanged, minus transports they never carried.
- `ports` is redundant once every answer read carries `declaredPorts`. It
  stays until then.
- A misspelled suffix (`53/upd`) fails the declaration with a `Problem` naming
  it, as a non-number does, rather than being read as TCP.

## Deferred

- **Removing `ports`.** Per 0118 §3 it takes two releases: make it optional,
  then stop sending it. Revisit when no supported release reads answers from
  an agent that predates `declaredPorts`, or when 0118's tolerant readers land
  and a CLI can be told to ignore it.
