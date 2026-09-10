# Agent Credential CLI Design

`discobox-access` is the in-sandbox client of the agent credentials
protocol. It asks a human for a credential the agent was not provisioned with,
and runs one command with it. That is the only way this CLI hands a credential
to anything: there is no command that returns a value with no command attached
to judge.

Decision record:
[ADR 0031](../docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md).
Protocol: [`docs/agent-credentials-protocol.md`](../docs/agent-credentials-protocol.md).

## Why it is its own module

It is meant to leave. The protocol is portable by design, so the client of it
should be liftable into another repository without dragging a runtime along.
That constrains one thing above all: **its only Go dependency is
`agentcreds`**, which is stdlib-only. Anything that would add a second package
dependency — reading sandbox config, knowing a mount path, importing
sandbox-agent — belongs on the server side of the protocol, not here.

`facts.go` (ADR 0090) is the one place this rule bends without breaking: it
shells out to `git`, a runtime dependency rather than a package one, and it is
optional in the way the rule demands — a repository this module is lifted into
that has no `git` on PATH, or is not a git checkout at all, gets no facts and
not an error. What the rule would not permit is reading sandbox config or a
mount path to find the facts; `git` is asked from wherever the process already
is, the same way `os.Getwd()` already was.

It is a real binary rather than an `argv[0]` alias of `discobox-sandbox-agent`
for the same reason. A multi-call binary is cheaper to ship and welds the client
to the runtime that happens to serve it today.

## Interface shape

The primary consumer is an LLM agent. The three operations do not have the same
shape, so they deliberately do not get the same interface.

| Operation | Input | Why |
| --- | --- | --- |
| `run` | argv after `--` | The declared command **is** the argv executed. Encoding it as JSON inserts a translation step between what the model wrote and what runs, and costs the child's exit status. |
| `request` | JSON on stdin (`--json`), or flags | Nested, and carries free text — a justification and use descriptions — through a shell that reads quotes and apostrophes as syntax. |
| `list` | nothing | — |

There used to be a fourth, `get`, that took a use id and printed the bare
value — for a script that could not be `exec`'d through `run`. It is gone from
this CLI ([ADR 0092](../docs/adr/0092-the-cli-has-no-unjudged-way-to-take-a-value.md)):
a value with no command attached to it is a value the judge never saw, and that
is not a capability this CLI keeps supporting just because dropping it is
inconvenient for one caller shape. The protocol call it used,
`POST /v1/credentials/use`, still exists — `run` calls it too, after judging —
because the gap in what can be secured today is the pool agent's, not this
CLI's to paper over by removing its own escape hatch and leaving the API's.

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

## The judge

`run` sends its argv and bounded working-directory/git context to the credential
service. It receives a sentinel only after the service judges the command and
records an allow. The CLI executes no model and accepts no caller verdict.
Unavailable, malformed, denied, or unrecorded results never start the child.

In Discobox the local sandbox-agent relays to the pool credential broker. The
broker loads the approved purpose from the control plane and calls the pool's
dedicated judge harness. The proxy calls the same service for each actual HTTP
request before substituting a use-scoped sentinel, including cached values and
retries. See [ADR 0106](../docs/adr/0106-a-trusted-judge-checks-requests-before-credential-substitution.md)
and [`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md).

`facts.go` gathers best-effort repository root, ref SHA, and commit subject.
These are untrusted caller claims; neither git metadata nor the declared
command grants authority over the actual request. Git lookups are bounded and
pin pager/external-diff settings. The portable client remains dependent only on
`agentcreds`; model selection, prompt construction and verdict persistence are
service responsibilities.

## The skill

`skills/discobox-access/SKILL.md` is what tells an agent this CLI exists. The
image installs it to `/usr/local/share/discobox/skills`, and the sandbox agent
copies it into the harness's skill directories on a sandbox's first launch
([ADR 0080](../docs/adr/0080-the-image-ships-the-skills-for-what-it-installs.md)).

It lives here, in the module whose interface it documents, for one reason: it
goes stale the moment a flag changes, and the only reliable defence is that it
sits where somebody changing that flag is already looking. **Change it in the
same commit that changes the interface.** It also travels — the instructions for
driving the CLI are part of what would leave with this module, while the
Dockerfile line and the install path, which are discobox's, stay behind.

## What it must never do

The value is opaque and short-lived. It goes into one child process's
environment — replacing rather than joining any same-named variable, so a stale
export cannot shadow it — and nowhere else: no file, no shell export, no log.
This CLI has no command that hands the value to a caller instead of a child
process, precisely because that caller could then do any of those.
