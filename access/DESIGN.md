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

`run` does not execute a command until a model has agreed the command is the use
a human approved it for ([ADR 0079](../docs/adr/0079-a-local-judge-gates-every-wrapped-credential-use.md)).

The model is reached through `discobox-prompt`, which the **harness image**
provides — `claude -p` for claude-code, `codex exec` for codex-cli — and which
the CLI calls as:

```
discobox-prompt --model judge --system <instructions> --prompt <use + argv + facts> --output-schema {allow, reason} --no-tools
```

`--model judge` names a role. The CLI never learns a model id; mapping the role
onto a model is the wrapper's job, because the wrapper is the half that knows
what its image installed. What the role is worth spending is its decision too:
claude-code sends `judge` to Sonnet and keeps the small model for `fast`, since
a wrong `allow` costs a credential and a wrong `deny` costs a retry.

`--no-tools` is passed on every call and cannot be turned off
([ADR 0090](../docs/adr/0090-the-judge-is-handed-facts-and-given-no-tools.md)).
The judge answers from its prompt and executes nothing: no command, no file
read, no network fetch. `discobox-prompt` maps it onto whatever its CLI calls
the same thing — claude-code adds `--tools "" --restricted
--disable-slash-commands`, which also stops the judge session reading the
sandbox's own `~/.claude` settings and skills, both of which the agent it is
judging can write; codex-cli, which has no tools-off switch, adds `--sandbox
read-only --ask-for-approval never` and leaves read access as the residual.

`judgePrompt` (`facts.go`) adds a small, bounded block of facts the CLI itself
gathers — never anything the argv or the repository chooses: the repository
root, and, for a command naming a git ref, that ref's resolved SHA and commit
subject. The subject is labelled in the prompt as the agent's own words
reaching the judge by a second route, not a fact about the world — it can only
catch a refspec that names something unlike the approved sentence, not verify
that a commit is what it claims. Every git call goes through `gitOutput`, which
pins `core.pager` and `diff.external` on the command line so the repository's
own configuration cannot redirect a lookup into running something else; nothing
here diffs or shows a patch today, so the guard is currently unreachable, but
it costs nothing and stays true if that changes.

Three properties do the work:

- **Judge first, take second.** The approved use comes from `list`, the verdict
  comes before the use call, and only then is a value issued. A refusal
  therefore mints no ephemeral sentinel and leaves no activation behind. A use
  `list` does not carry is refused without asking anyone: there is no approved
  sentence to hold the command up to.
- **Every non-yes is a no.** No wrapper installed, a non-zero exit, an
  unreadable answer, or `allow: false` all stop the command with code `denied`.
  There is no flag to skip the judge — an escape hatch would be used by exactly
  the situation the gate exists for.
- **A credential is never issued without a record of why**
  ([ADR 0091](../docs/adr/0091-a-credential-is-not-issued-without-a-verdict-on-record.md)).
  `judgeCommand` always returns the verdict it reached, not only whether to
  proceed, and `run` carries it on the very call that takes a value —
  `agentcreds.UseBody.Verdict` — so the pool agent can persist it to the
  control plane before minting and refuse to mint at all if that write fails.
  A refusal never reaches that call, so it has nowhere else to leave a trace;
  `run` reports it separately with `client.ReportDenial`, best-effort — its own
  failure changes nothing about what `run` reports for the refusal that
  prompted it — and skipped entirely when no judge was ever reached (a missing
  wrapper, say): there is no verdict to report when nothing was asked.

**It is a guardrail, not a boundary.** It runs in the sandbox, so an agent that
does not want to be judged calls the protocol endpoint directly, bypassing this
CLI entirely. Nothing here is an authorization decision and no trusted-side
check is relaxed for it: the pool agent still refuses a sentinel outside its
window or against another host, and the control plane still checks the grant.
What the judge catches is the honest failure — drift from the granted task, a
command broader than it needed, or an agent steered by something it read.

This CLI no longer offers a way to take a value without a command to judge.
There was one, `get`, and removing it was a decision made in advance of it ever
being made safe: an unjudged path that any future improvement to the judge
would have to keep supporting is a path that has to be secured forever, not
just today. See [ADR 0092](../docs/adr/0092-the-cli-has-no-unjudged-way-to-take-a-value.md).

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
