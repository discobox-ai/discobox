# 0110 — Whoever serves an API is strict, and whoever reads its responses is tolerant

- **Status**: Proposed
- **Date**: 2026-09-11
- **Deferred**: every API stays strict until the condition in
  [When to take this up](#when-to-take-this-up) holds.

## Context

All three APIs are strict in both directions. Every object schema in
`api/openapi/server.yaml` (134), `pool-agent/api/openapi/pool.yaml` (23), and
the `sandbox.yaml` derived from the first (25) says
`additionalProperties: false`. ogen turns that into a decoder that fails the
whole payload on any field it does not declare: `denyAdditionalProps` is set
from the schema alone (ogen's `schema_gen.go`), and the decoder returns
`unexpected field %q`. Not one generated decoder skips an unknown field. The
strictness dates from the contracts module split (2026-06-16), and no
document records why.

One generated package per spec decodes both directions. `(*Origin).Decode`
decodes a create request on the server and a listed sandbox in the CLI, so
whatever the schema says applies to both:

| Spec | Package | Serves it | Reads its responses |
| --- | --- | --- | --- |
| `server.yaml` | `api/gen`, aliased by `api/model` | control plane | CLI; pool-agent, for sandbox-agent status typed by it |
| `sandbox.yaml` | `api/sandboxgen` | sandbox-agent | control plane (`sshd/session.go`) |
| `pool.yaml` | `pool-agent/api/gen` | pool-agent | control plane (`poolruntime/agent_client.go`) |

On the serving side, strictness is what we want. A request carrying a field
the server does not know is a request it cannot honor as asked, and refusing
it is better than silently doing something else.

On the reading side, it means a field added to a response breaks every older
reader of it. That is accepted today, on purpose: compatibility across
versions is not maintained, and a client that breaks is the signal to upgrade
it. The CLI replaces an older local server that it starts (`cli/DESIGN.md`), so
the common pairing is never mismatched.

It already costs something wherever a reader cannot simply be upgraded:

- `sandboxPrimaryTerminalTitle` (`services/helpers.go`) and `reportedLastAccess`
  (`pools/agent_service.go`) decode sandbox-agent status through hand-written
  local structs. The generated types would fail the payload over a field
  nothing reads, and a stopped sandbox's row is never rewritten, so the loss
  would be permanent.
- `pool-agent/statuspoll.go` decodes a sandbox-agent's resource usage through
  the strict `apimodel.SandboxAgentResourceUsage`. A sandbox-agent that adds a
  field to it breaks that poll for its sandbox — and the agent inside a running
  sandbox is the one component nobody upgrades by installing a new binary.

## Decision

### 1. The serving side stays strict

Each spec as written keeps `additionalProperties: false`. The control plane,
the sandbox-agent, and the pool-agent go on decoding requests with the package
generated from it.

### 2. The reading side decodes from a tolerant copy of each spec

A generator derives each spec's tolerant copy by deleting every
`additionalProperties: false`. It deletes rather than setting `true`, because
`true` makes ogen add a catch-all map field to every type. With the property
absent, ogen's decoder skips an unknown field (`d.Skip()`). Each copy generates
into a client-only package (ogen's `paths/server` feature disabled), aliased as
`api/model` is:

- `server.yaml`'s copy for the CLI, and for the pool-agent's reading of
  sandbox-agent status.
- `sandbox.yaml`'s copy for the control plane's sandbox-agent client.
- `pool.yaml`'s copy for the control plane's pool-agent client.

This is the pattern `gensandboxopenapi` already follows to derive
`sandbox.yaml` from `server.yaml`. `generate:verify` keeps each copy in step
with its spec, so a spec and its copy can differ only in strictness.

### 3. Required stays required

Tolerance covers fields a reader does not know, not fields it expects and does
not get: ogen checks `required` separately. Removing a field from a response is
therefore two releases — make it optional, then stop sending it once the
readers that required it are gone. On the request side, strictness means a
field an older client may still send stays in the schema as accepted and
ignored.

### 4. Decodes that went around the generated types come back to them

The local structs in `services/helpers.go` and `pools/agent_service.go` are
replaced by the tolerant generated types, and `pool-agent/statuspoll.go` reads
through them.

## When to take this up

When a reader can no longer be upgraded in step with what it reads. Any one of
these is enough:

- CLIs are expected to keep working against a server newer than they are: a
  shared or remote server, several people's CLIs, or a release that promises
  compatibility.
- Pool-agents or sandbox-agents routinely run a different release from the
  control plane for longer than an upgrade takes — for example, long-lived
  sandboxes keeping the agent they started with.
- A third decode is hand-rolled around a generated type for this reason. That
  is the cost showing up one breakage at a time.

Until then, a breaking client is the signal to upgrade, which is the behavior
wanted.

## Alternatives rejected

**Drop `additionalProperties: false` everywhere.** The server would then accept
a field it does not understand and act on the request without it. The serving
side's strictness is the half worth keeping.

**One tolerant package, with strict checking in server middleware.** It means a
second pass over every request body against the spec, outside the generated
code the server is built on — a validator dependency or a hand-walked schema,
and two sources of truth for what a request may contain. That is more
machinery than a second ogen run.

**An ogen setting.** There is none. `denyAdditionalProps` comes only from the
schema, and ogen's features toggle paths, validation, and instrumentation, not
decoding strictness.

**Hand-written local structs wherever it hurts.** That is today's answer. It
works one decode at a time, and each one is found by a breakage.

## Consequences

- The generated code for the three APIs roughly doubles.
- The CLI's and the pool-agent's imports move to the client packages. It is
  import lines only: the types keep their names and shapes.
- One binary can hold both copies of a spec's types — the control plane serves
  `server.yaml` and reads agent-written blobs that `server.yaml` types. They are
  distinct Go types and are never converted between: a handler uses the strict
  one, and a reader the tolerant one.
- It helps only readers built after it lands. Readers already deployed stay
  strict until they are replaced.
