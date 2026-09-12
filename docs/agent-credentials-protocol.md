# Agent Credentials Protocol v1

A small HTTP protocol an agent inside a sandbox uses to **ask a human** for a
credential it was not provisioned with, and then **use** it. Four operations:
`list`, `request`, `get`, and reporting a denial `get` never saw.

The protocol knows nothing about Discobox. It is the contract between the
in-sandbox CLI (`discobox-access`) and whatever serves it, so the same CLI
works against Discobox's sandbox-agent and against any other implementation.

Decision record:
[ADR 0031](adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md).

## Transport

- HTTP/1.1, JSON request and response bodies, all routes under `/v1`.
- The client is configured with a **base URL** (`DISCOBOX_CREDENTIALS_URL`,
  defaulting to `http://127.0.0.1:17010`) and an optional **bearer token**
  (`DISCOBOX_CREDENTIALS_TOKEN`). When a token is configured it is sent as
  `Authorization: Bearer <token>` on every request. Implementations reachable
  only over a trusted local transport — Discobox serves the protocol on sandbox
  loopback — may require no token. Discobox sets `DISCOBOX_CREDENTIALS_URL` to
  that default in every exec's environment unless the exec already names one,
  and sets no token.

### Errors

A failure is the HTTP status plus a body carrying a **stable code** and a
message:

```json
{ "code": "denied", "error": "no live approved use use_7f3c…" }
```

| Code | Status | What the caller should do |
| --- | --- | --- |
| `invalid` | 400 | The call was malformed. Fix it and retry. |
| `denied` | 403 | You may not do this. Ask for it with `request`. |
| `not_found` | 404 | The id means nothing to this server. |
| `unavailable` | 5xx | The server could not answer. Retry; the same call may succeed. |

The body's `code` wins over the status line. A client reading an error with no
`code` classifies it by status: 400 or 409 is `invalid`, 401 or 403 `denied`,
404 `not_found`, anything else `unavailable`.

The code exists so a caller — an agent, above all — can branch on a token
rather than on the wording of a sentence. The set is deliberately coarse: an id
that is unknown, revoked, or expired all report `denied`, because saying which
would tell an untrusted caller more about the state of an approval than it needs
to know, and the remedy is the same for all three.

The message never repeats the code. They are two halves of one answer.

## The value a `get` returns is not necessarily a secret

`get` returns **a value to place in an environment variable**, not a promise
about what that value is. An implementation may return the real credential. The
Discobox implementation deliberately returns an *ephemeral sentinel* — a
byte-shaped lookalike that means nothing outside a short activation window, and
that the egress proxy exchanges for the real credential on the way out.

A client must therefore treat the value as opaque: pass it to the child process
and do not parse, log, persist, or reuse it after `expiresAt`.

## `list` — what am I allowed to use?

```
GET /v1/credentials
```

```json
{
  "credentials": [
    {
      "name": "github",
      "envVar": "GITHUB_TOKEN",
      "host": "api.github.com",
      "uses": [
        {
          "useId": "use_7f3c…",
          "description": "Open a pull request against the current repository",
          "expiresAt": "2026-08-12T18:00:00Z"
        }
      ]
    }
  ]
}
```

`list` never returns values. `expiresAt` is when the approval behind the use
lapses; an absent `expiresAt` means it does not expire on its own.

## `request` — ask for something new

```
POST /v1/credentials/requests
```

```json
{
  "name": "github",
  "envVar": "GITHUB_TOKEN",
  "host": "api.github.com",
  "justification": "The task asks me to open a PR with the fix.",
  "uses": [{ "description": "Open a pull request against the current repository" }],
  "grantTTLSeconds": 14400
}
```

```json
{ "requestId": "req_1a2b…", "status": "pending" }
```

`grantTTLSeconds` is optional: how long the agent asks the approval to last. It
is a suggestion, not a term — Discobox shows it to the approver as the answer
already chosen, and they may choose any other. Absent or `0` asks for nothing in
particular, so forever cannot be asked for: the approver may still grant it, but
the one answer that never comes back to be asked again is never the default.

An ask runs from 1 second to **2592000 (thirty days)**; outside that range it is
`invalid`. The ceiling is part of the contract rather than a Discobox limit,
because an ask is only worth anything if it can be shown to a human as an
answer: a ten-year ask is one keystroke from a credential that outlives the
work, and a number large enough to overflow the duration a client converts it to
lands back near zero, which reads as forever.

`host` is the destination the credential will be sent to. It is required by the
Discobox implementation, which refuses to mint a host-unscoped approval through
this flow. Discobox also requires `name`, a valid `envVar`, and at least one use
with a description, and answers `invalid` without them. A second ask for the
same `envVar` and `host` while one is still pending returns that pending
request rather than a new one.

Approval is human-latency, so `request` is **asynchronous**: it answers
`202 Accepted` immediately and the client polls.

```
GET /v1/credentials/requests/{requestId}
```

```json
{ "requestId": "req_1a2b…", "status": "granted", "uses": [{ "useId": "use_7f3c…", "description": "…" }] }
```

`status` is one of `pending`, `granted`, `denied`. `uses` is present once
granted and carries the ids `get` accepts — the approver may have edited the
descriptions, so the granted uses are authoritative, not the requested ones.
Discobox reports an approval whose grant has since been revoked as `denied`,
and a request id that is not the calling sandbox's own as `not_found`.

Blocking is the client's job (`--wait` on the CLI), built on this poll. There is
no long-poll and no synchronous request primitive.

## `get` — take a value for one command

```
POST /v1/credentials/use
```

```json
{
  "useId": "use_7f3c…",
  "command": ["gh", "pr", "create", "--fill"],
  "verdict": { "allow": true, "reason": "opens a PR against the approved repo", "role": "judge", "prompt": "Approved use: …", "latencyMs": 842 }
}
```

```json
{ "envVar": "GITHUB_TOKEN", "value": "ghp_…", "expiresAt": "2026-08-12T17:05:00Z" }
```

The caller **declares the command it is about to run**. The declaration is
recorded on the serving side before the value is handed out; it narrows the
window and gives the audit log a per-use story. It is not a trust anchor: in
Discobox the real enforcement happens against the actual outbound request at
swap time, and a client that lies about its command gains nothing.

`verdict` is required
([ADR 0091](adr/0091-a-credential-is-not-issued-without-a-verdict-on-record.md)):
a caller reports what decided the command was the approved use, and an
implementation that judges its callers persists it before the value is handed
out, so a credential is never issued with no record of why. `role` names what
answered (a role like `"judge"`, never a vendor model id — a caller with
nothing that decided the command, because it never judges its callers at all,
reports the role it would have asked for anyway, such as `"none"`). `prompt`
is the exact text the decision was made from, in full. An implementation that
makes no such decision may still require the field and record it verbatim; the
protocol does not make persistence itself mandatory, only that the field is
sent. Discobox answers `invalid` when `useId` is missing or the verdict has no
`role` or `prompt`, and records the verdict to the control plane before it
mints: if that write fails, no value is issued.

`expiresAt` is the end of this value's window. A client that needs the
credential again after it passes calls `get` again rather than holding the value.

## Reporting a verdict `get` never saw

```
POST /v1/credentials/denials
```

```json
{
  "useId": "use_7f3c…",
  "command": ["curl", "-X", "DELETE", "…"],
  "verdict": { "allow": false, "reason": "broader than the approved use", "role": "judge", "prompt": "Approved use: …" }
}
```

A caller that judges its own commands before calling `get` (`discobox-access`
does; the protocol does not require it) never calls `get` at all for a command
its judge refused — there is nothing to issue, so there is nothing for `get`'s
own recording to catch. Without this operation that verdict would exist only
on the caller's own side, if anywhere. A report the server accepts answers
`204`, whether the verdict allowed or refused the command; Discobox answers
`invalid` for one with no `useId` or with no verdict `role` or `prompt`.
Reporting it is the caller's choice, not its obligation, and a client is free
to treat this call's own failure as unremarkable — it is what a caller
volunteers about a decision made before this protocol was ever asked to act on
it, not a correction to something `get` returned.

## The client shape that fits it best

The reference client is `discobox-access`. Its primary consumer is an LLM
agent, and the operations do not all have the same shape, so they deliberately
do not get the same interface.

**Wrapping a command takes argv**, because the command it executes *is* the
command it declares — encoding it as JSON would put a translation step between
the two, and cost the child's exit status:

```
discobox-access run --use <useId> -- gh pr create --fill
```

The returned value is injected into that child process's environment only —
never into the shell, a dotfile, or a file on disk — replacing rather than
joining any same-named variable, so a stale export cannot shadow it.

**Asking takes JSON**, because the ask is nested and carries free text through a
shell that reads quotes and apostrophes as syntax:

```
discobox-access request --json <<'EOF'
{
  "name": "github",
  "envVar": "GITHUB_TOKEN",
  "host": "api.github.com",
  "justification": "the user's task asks me to open a PR",
  "uses": [{"description": "Open a PR against the current repo"}],
  "wait": true
}
EOF
```

`--json` means "talk to me in JSON" in whichever direction a command has:
structured output everywhere, and a structured body on stdin for `request`.
Results go to stdout and failures to stderr, always — which is what lets `run`
hand its child the real stdout untouched.

**The reference client judges before it runs.** Before executing a wrapped
command, `discobox-access` asks a local model whether the command is the use
it was approved for, and refuses to start it otherwise
([ADR 0079](adr/0079-a-local-judge-gates-every-wrapped-credential-use.md)). That
is a property of this client, not of the protocol: an implementation serving the
protocol neither knows nor depends on whether its caller does this, and a
different client may do something else.

**The reference client has no unwrapped way to take a value.** The wire
operation above is `get` for a reason — a caller of the protocol may still ask
for a value with no command attached — but `discobox-access` itself dropped
that as a CLI capability
([ADR 0092](adr/0092-the-cli-has-no-unjudged-way-to-take-a-value.md)): a value
with nothing for the judge to have ruled on is exactly the case the judge
cannot help with, and this client does not keep offering one just because
wrapping is sometimes inconvenient. `list` and the flag form of `request`
remain, for scripting the parts that were never about a value in the first
place.

## Implementing the server side

An implementation owns four decisions the protocol does not make:

1. **Who may call it.** The protocol carries no identity beyond the optional
   bearer token; the transport decides whose credentials these are.
2. **What `get` returns.** Real value, or a scoped stand-in.
3. **How a request reaches a human.** The protocol only says a request has an
   id and a status that eventually settles.
4. **What becomes of a verdict.** The field is required on both `get` and the
   denial report; whether either is persisted, and where, is not specified.

In Discobox: sandbox-agent serves the protocol on sandbox loopback
(`127.0.0.1:17010`) and relays each call to the pool over the sandbox's mTLS
client certificate, whose common name is the sandbox ID and is the identity.
The pool's proxy unit (`pool-agent/proxyagent`, on `:17083`) answers it: it
calls the control plane, where grants and cleartext live, and mints the
ephemeral sentinel. See
[`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md) and
[`sandbox-agent/DESIGN.md`](../sandbox-agent/DESIGN.md).
