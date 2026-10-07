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
sandbox-agent — belongs on the server side of the protocol, not here. The CLI
learns where it runs only from the environment variables `agentcreds` names:
`DISCOBOX_CREDENTIALS_URL` (default `http://127.0.0.1:17010`) and
`DISCOBOX_CREDENTIALS_TOKEN`.

`run` bends it once more, for its child only: when `DISCOBOX_SERVER` is unset
it gives the child `DISCOBOX_SERVER=$DISCOBOX_API_URL`, the discobox API the
pool names. A development shell in a discobox unsets `DISCOBOX_SERVER` to
reach its local server, and the discobox CLI run there under a use of
`ai.discobox.sandbox` would otherwise start a server of its own. A server
already named is kept, and without `DISCOBOX_API_URL` nothing is added.

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

The primary consumer is an LLM agent. The operations do not have the same
shape, so they deliberately do not get the same interface.

| Operation | Input | Why |
| --- | --- | --- |
| `run` | argv after `--`, and whatever the command reads on stdin | The declared command **is** the argv executed. Encoding it as JSON inserts a translation step between what the model wrote and what runs, and costs the child's exit status. A command that takes its request on stdin (`discobox new --json`, `gh api --input -`) is judged with it: see the judge, below. |
| `request` | JSON on stdin (`--json`), or flags, after an optional well-known ID | Nested, and carries free text — a justification and use descriptions — through a shell that reads quotes and apostrophes as syntax. A well-known ID (`com.github.api`) stands in for the name, variable, and host, which the implementation fills from the root `wellknown` registry. |
| `list` | nothing | — |
| `trust` | a host argument and flags, or JSON on stdin (`--json`) | The protocol's trust verb (ADR 0149): ask for a host whose certificate the egress refuses to be trusted for this sandbox. It carries free text for the reason `request` does. Nothing is run under it, so there is nothing to judge here; the proxy judges every request to the trusted host against its uses. |
| `trusts` | nothing | — |
| `wait` | `request` or `trust`, then an existing request ID; optional `--json` and `--timeout` flags first | Resume polling without creating another request. The explicit kind keeps service-owned ID formats out of the CLI. |

There is no command that takes a use id and prints the bare value
([ADR 0092](../docs/adr/0092-the-cli-has-no-unjudged-way-to-take-a-value.md)):
a value with no command attached to it is a value the judge never saw. The
protocol's use call, `POST /v1/credentials/use`, is what `run` calls, and it
carries the command: a caller that reaches it without this CLI still has its
declared command judged before anything is minted, because the judging happens
on the pool's side of the call, not here.

`--json` means **"talk to me in JSON"** for whichever direction a command has:
structured output everywhere, plus a structured body on stdin for `request`.
It is one flag rather than separate input and output switches because an agent
that wants structure wants it in both directions.

Rules the shape depends on:

- **Results to stdout, failures and progress to stderr, always.** `run` hands the child the
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
- **`--debug` shows the wire, never a value.** Given before the command, it
  prints every call to the credentials service on stderr: method and URL, the
  JSON request body, the status, and the response body (`debug.go`, a
  round-tripper given to `agentcreds.WithHTTPClient`). No header is printed,
  so no token is, and a use response's `value` is printed as `<redacted>`; a
  use response it cannot read as a JSON object is not shown at all. A debug
  line that carried the value would be the unjudged way to take one that
  [ADR 0092](../docs/adr/0092-the-cli-has-no-unjudged-way-to-take-a-value.md)
  rules out. The lines are plain text even under `--json`.
- **Exit status is meaningful.** `0` success, `1` the call failed (a missing
  `--use` included: it is reported by code `invalid`), `2` the invocation could
  not be parsed (bad flags, no command, an unreadable `--json` body), and for
  `run` the child's own status passes straight through, as `env`(1) does. A
  `--wait` that settles as *denied* exits non-zero: the call completed, but the
  answer was no, and a shell-driven agent reads a zero as approval.

## Approval waiting

`request --wait`, `trust --wait`, and `wait` keep polling until the request
settles or the wait context ends. They emit a pending notice before polling,
also in JSON mode: a `progress` envelope on stderr carries the request kind,
ID, whether this process is waiting, and instructions to keep monitoring the
execution across tool yields. stdout remains a single result. A request made
without waiting emits the same notice with the exact command to resume it.
Timeouts end the local wait, not the service-owned request. Resuming a trust
request returns the status available from the service; only the original
`trust --wait` can preserve certificate-chain metadata from its creation call.

The bundled skill requires the agent to keep its turn active while waiting
and resume the authorized work after approval without another user prompt.

## The judge

`run` does not execute a command until the service hands it a value, and
Discobox's service hands one out only once the project's judge has allowed the
command for the use
([ADR 26-09-22-838](../docs/adr/26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md) §3).
This CLI judges nothing itself: it runs no model, decodes no verdict, and
reports none. It sends evidence on the use call (`agentcreds.UseBody`) and runs
the command only if a value comes back.

What it sends beside the use ID and the argv:

- **What the command will read on stdin** (`stdin.go`,
  [ADR 26-09-27-905](../docs/adr/26-09-27-905-the-command-judge-is-shown-a-bounded-stdin.md)),
  because for a command that takes its request there, the argv says nothing
  about what it does. When fd 0 is a file or a pipe, `run` reads up to
  `maxJudgedStdin` (8 KiB, the most of an input the judge accepts, and small
  enough that the body fits one protocol call however JSON escapes it), waiting
  at most `stdinArrivalWait` (5 s) for it to end, and sends it as
  `agentcreds.Stdin`: the text shown, and a sentence for whatever was not —
  past the bound, still arriving, not text, a failed read. The child reads
  exactly what was sent: the bytes read for the judge as they arrive, then the
  rest of fd 0. It gets them through an OS pipe `run` feeds rather than a reader
  exec copies, because exec waits for that copy: a writer holding stdin open
  would hold `run` open after its command exited. A terminal, a socket and a
  character device are passed through unread: an agent's shell tool hands its
  commands an open socket that never ends, and a terminal is a person.
- **Where the command runs** (`facts.go`,
  [ADR 0090](../docs/adr/0090-the-judge-is-handed-facts-and-given-no-tools.md)),
  as `agentcreds.Reported`: the working directory, the repository root, and for
  a `git` command naming a ref, that ref's resolved commit and subject line.
  Each is best-effort, bounded by `factsTimeout` (5 s) combined, cut to
  `agentcreds.MaxReportedBytes`, and left out when it could not be established.
  The judge is told all of it is the discobox's claim; a subject line is the
  agent's own words. Every git call goes through `gitOutput`, which pins
  `core.pager` and `diff.external` on the command line so the repository's own
  configuration cannot redirect a lookup into running something else.

The use call waits up to `agentcreds.UseTimeout`, since a judge that has to be
brought up first takes minutes. Every refusal — the judge's, a judge that could
not be reached, a use no longer live — comes back as code `denied` with the
reason, and the command never starts. There is no flag to skip the judge, and
no local fallback: whether commands are judged at all is the server's setting
(`judgeCommands`, on by default,
[ADR 26-10-02-054](../docs/adr/26-10-02-054-commands-are-judged-by-default-and-requests-by-opt-in.md)),
not the sandbox's.

**What it is and is not.** The judge, its model and its prompt are on trusted
ground, and minting is gated by its allow, so an agent cannot replace the judge,
rewrite its settings, or forge a verdict — and the verdict is recorded by the
control plane before the pool mints anything
([ADR 0091](../docs/adr/0091-a-credential-is-not-issued-without-a-verdict-on-record.md)).
But the argv, stdin and facts are still the sandbox's word: nothing binds the
process that receives the value to the command that was judged. What sees the
request that actually carries the credential is the proxy's request judge,
which a server opts into (`judgeCredentials`); without it, the pool still holds
every sentinel to its grant's hosts and its activation's window.

This CLI has no way to take a value without a command to judge
([ADR 0092](../docs/adr/0092-the-cli-has-no-unjudged-way-to-take-a-value.md)):
an unjudged path is one every future improvement to the judge would have to
keep supporting, and so one that has to be secured forever, not just today.

## The skill

`skills/discobox-access/SKILL.md` is what tells an agent this CLI exists. The
image installs it to `/usr/local/share/discobox/skills`, and the sandbox agent
copies it into the harness's skill directories on the primary terminal's
first launch ([ADR 0080](../docs/adr/0080-the-image-ships-the-skills-for-what-it-installs.md)).

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
