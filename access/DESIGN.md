# Agent Credential CLI Design

`discobox-access` is the in-sandbox client of the agent credentials
protocol. It asks a human for a credential the agent was not provisioned
with, or for a host whose certificate the egress refuses to be trusted, and
reports when the answer comes. It never carries a credential's value: a
granted credential is delivered by the service — Discobox puts a sentinel in
the variable the request named, and its egress swaps it for the value on the
routes its secret and its binding allow ([ADR 26-10-27-880](../docs/adr/26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)).
So the agent uses a granted credential by running the command that needs it;
there is nothing to wrap.

Decision records:
[ADR 0031](../docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
narrowed by ADR 26-10-27-880, which removed `run`, the judge, policies, uses,
well-known IDs and `facts`. The protocol's redesign is deferred to a later ADR;
this is the minimal client of it until then.
Protocol: [`docs/agent-credentials-protocol.md`](../docs/agent-credentials-protocol.md).

## Why it is its own module

It is meant to leave. The protocol is portable by design, so the client of it
should be liftable into another repository without dragging a runtime along.
That constrains one thing above all: **its only Go dependency is the protocol
package, `agentcreds`**, which is standard library only. Anything that would
add a dependency — reading sandbox config, knowing a mount path, importing
sandbox-agent — belongs on the server side of the protocol, not here. The CLI
learns where it runs only from the environment variables `agentcreds` names:
`DISCOBOX_CREDENTIALS_URL` (default `http://127.0.0.1:17010`) and
`DISCOBOX_CREDENTIALS_TOKEN`.

It is a real binary rather than an `argv[0]` alias of `discobox-sandbox-agent`
for the same reason. A multi-call binary is cheaper to ship and welds the client
to the runtime that happens to serve it today.

## Interface shape

The primary consumer is an LLM agent. The operations do not have the same
shape, so they deliberately do not get the same interface.

| Operation | Input | Why |
| --- | --- | --- |
| `list` | nothing | The credentials the sandbox holds: name, the variable it is delivered in, the hosts of its routes, its lifetime, and whether it is a delegation (delivered nowhere; given to the discoboxes the sandbox creates). |
| `request` | flags (`--name`, `--env-var`, `--route`, `--hosts`, `--justification`, `--ttl`, `--delegate`, `--wait`, `--timeout`), or JSON on stdin (`--json`) | Carries free text — a justification — through a shell that reads quotes and apostrophes as syntax. Name, at least one route or host, and — to use it, never to delegate it — a variable are required and refused here before anything is sent. A route is the typed form `[METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][=blocked]`, one per `--route` since a comma is the route's own, sent as written: the service parses them and refuses one that does not parse as `invalid`, naming it, which this client prints as it prints any refusal — it has no route parser of its own, since that would be a dependency (above). The grammar in its help is a copy of `secretroute.Syntax`, which the CLI's conventions test holds it to. A host is exact (`*.h` for the hosts beneath h), and `--hosts` is shorthand for one route to each. |
| `trust` | a host argument and flags (`--justification`, `--ca-file`, `--ttl`, `--wait`), or JSON on stdin (`--json`) | The protocol's trust verb (ADR 0149): ask for a host whose certificate the egress refuses to be trusted for this sandbox. A trust only pins a certificate for that one endpoint; what is sent there is not its question (ADR 26-10-27-880 §10). |
| `env` | nothing | The protocol's environment verb: every credential variable and what is delivered in it now, as `export` lines a POSIX shell evaluates (`eval "$(discobox-access env)"`), each value single-quoted so the shell reads none of it. A harness's tool shell inherits the environment the harness was launched with, so a credential granted mid-session reaches the agent only this way. The values are sentinels, never credentials. |
| `trusts` | nothing | — |
| `audit` | filter flags (`--host`, `--blocked`/`--allowed`, `--since`, `--limit`) | The protocol's audit verb ([ADR 26-10-02-678](../docs/adr/26-10-02-678-a-sandbox-reads-its-own-egress-audit-through-the-access-protocol.md)): the sandbox's own outbound requests as the egress recorded them, with the reason for each refusal. It is for the failure a program swallowed; the reason says whether to `request`, `trust`, or send the credential somewhere its routes allow. |
| `wait` | `request` or `trust`, then an existing request ID; optional `--json` and `--timeout` flags first | Resume polling without creating another request. The explicit kind keeps service-owned ID formats out of the CLI. |

There is no command that returns a value: no protocol call carries one.

`--json` means **"talk to me in JSON"** for whichever direction a command has:
structured output everywhere, plus a structured body on stdin for `request`
and `trust`. It is one flag rather than separate input and output switches
because an agent that wants structure wants it in both directions.

Rules the shape depends on:

- **Results to stdout, failures and progress to stderr, always.**
- **The JSON body replaces the flags; it does not merge with them.** Two
  sources for one field is a silent-precedence bug waiting to be reported as
  "it ignored my justification".
- **Unknown JSON fields are rejected.** A misspelled key that is silently
  dropped surfaces much later, as a human asking why the request had no
  justification. A field a request no longer carries — `uses`, `statements`,
  `id` — is refused the same way, so an agent working from an old example
  learns at once.
- **Failures carry a stable code**, so an agent branches on a token rather than
  on wording. `Code` and `Message` both come from `agentcreds` — the CLI does
  not invent its own classification, and the message never repeats the code.
- **`--debug` shows the wire.** Given before the command, it prints every call
  to the credentials service on stderr: method and URL, the JSON request body,
  the status, and the response body (`debug.go`, a round-tripper given to
  `agentcreds.WithHTTPClient`). No header is printed, so no token is. The
  lines are plain text even under `--json`.
- **Exit status is meaningful.** `0` success, `1` the call failed, `2` the
  invocation could not be parsed (bad flags, a missing name, variable or
  host, an unreadable `--json` body). A `--wait` that settles as *denied*
  exits non-zero: the call completed, but the answer was no, and a
  shell-driven agent reads a zero as approval.
- **A lifetime is asked for, never set.** `--ttl` (`grantTTLSeconds`) is the
  answer a human is shown already chosen; a fraction of a second, a negative,
  or one over thirty days is refused rather than rounded or trimmed.

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

A granted request reports the credential as the approver chose it — the routes
and lifetime may differ from the ask — and that it is in its variable for
every command started from then on.

The bundled skill requires the agent to keep its turn active while waiting
and resume the authorized work after approval without another user prompt.

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
