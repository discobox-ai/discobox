# Agent Credential CLI Design

`discobox-credential` is the in-sandbox client of the agent credentials
protocol. It asks a human for a credential the agent was not provisioned with,
and runs one command with it.

Decision record:
[ADR 0031](../docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md).
Protocol: [`docs/agent-credentials-protocol.md`](../docs/agent-credentials-protocol.md).

## Why it is its own module

It is meant to leave. The protocol is portable by design, so the client of it
should be liftable into another repository without dragging a runtime along.
That constrains one thing above all: **its only dependency is `agentcreds`**,
which is stdlib-only. Anything that would add a second dependency — reading
sandbox config, knowing a mount path, importing sandbox-agent — belongs on the
server side of the protocol, not here.

It is a real binary rather than an `argv[0]` alias of `discobox-sandbox-agent`
for the same reason. A multi-call binary is cheaper to ship and welds the client
to the runtime that happens to serve it today.

## Interface shape

The primary consumer is an LLM agent. The four operations do not have the same
shape, so they deliberately do not get the same interface.

| Operation | Input | Why |
| --- | --- | --- |
| `run` | argv after `--` | The declared command **is** the argv executed. Encoding it as JSON inserts a translation step between what the model wrote and what runs, and costs the child's exit status. |
| `request` | JSON on stdin (`--json`), or flags | Nested, and carries free text — a justification and use descriptions — through a shell that reads quotes and apostrophes as syntax. |
| `list` | nothing | — |
| `get` | one id | Flags are already precise. |

`--json` means **"talk to me in JSON"** for whichever direction a command has:
structured output everywhere, plus a structured body on stdin for `request`.
It is one flag rather than separate input and output switches because an agent
that wants structure wants it in both directions.

Rules the shape depends on:

- **Results to stdout, failures to stderr, always.** `run` hands the child the
  real stdout, so the wrapper must never write into it.
- **The JSON body replaces the flags; it does not merge with them.** Two
  sources for one field is a silent-precedence bug waiting to be reported as
  "it ignored my justification".
- **Unknown JSON fields are rejected.** A misspelled key that is silently
  dropped surfaces much later, as a human asking why the request had no
  justification.
- **Failures carry a stable code**, so an agent branches on a token rather than
  on wording. `Code` and `Message` both come from `agentcreds` — the CLI does
  not invent its own classification, and the message never repeats the code.
- **Exit status is meaningful.** `0` success, `1` the call failed, `2` the
  invocation was wrong, and for `run` the child's own status passes straight
  through, as `env`(1) does. A `--wait` that settles as *denied* exits non-zero:
  the call completed, but the answer was no, and a shell-driven agent reads a
  zero as approval.

## What it must never do

The value is opaque and short-lived. It goes into one child process's
environment — replacing rather than joining any same-named variable, so a stale
export cannot shadow it — and nowhere else: no file, no shell export, no log.
`get` exists for scripts that cannot be wrapped, and is the weaker path
precisely because it hands the value to a caller that may do any of those.
