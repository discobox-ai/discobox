# Agent Credentials Protocol v1

A small HTTP protocol an agent inside a sandbox uses to **ask a human** for a
credential it was not provisioned with, and then **use** it. Three operations:
`list`, `request`, `get`.

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
  loopback — may require no token.

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
  "uses": [{ "description": "Open a pull request against the current repository" }]
}
```

```json
{ "requestId": "req_1a2b…", "status": "pending" }
```

`host` is the destination the credential will be sent to. It is required by the
Discobox implementation, which refuses to mint a host-unscoped approval through
this flow.

Approval is human-latency, so `request` is **asynchronous**: it returns
immediately and the client polls.

```
GET /v1/credentials/requests/{requestId}
```

```json
{ "requestId": "req_1a2b…", "status": "granted", "uses": [{ "useId": "use_7f3c…", "description": "…" }] }
```

`status` is one of `pending`, `granted`, `denied`. `uses` is present once
granted and carries the ids `get` accepts — the approver may have edited the
descriptions, so the granted uses are authoritative, not the requested ones.

Blocking is the client's job (`--wait` on the CLI), built on this poll. There is
no long-poll and no synchronous request primitive.

## `get` — take a value for one command

```
POST /v1/credentials/use
```

```json
{ "useId": "use_7f3c…", "command": ["gh", "pr", "create", "--fill"] }
```

```json
{ "envVar": "GITHUB_TOKEN", "value": "ghp_…", "expiresAt": "2026-08-12T17:05:00Z" }
```

The caller **declares the command it is about to run**. The declaration is
recorded on the serving side before the value is handed out; it narrows the
window and gives the audit log a per-use story. It is not a trust anchor: in
Discobox the real enforcement happens against the actual outbound request at
swap time, and a client that lies about its command gains nothing.

`expiresAt` is the end of this value's window. A client that needs the
credential again after it passes calls `get` again rather than holding the value.

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

`list`, a bare `get`, and the flag form of `request` exist for scripting.
`get` is the weaker path on purpose: it hands the value to a caller that may
log or persist it, which the wrapper structurally cannot.

## Implementing the server side

An implementation owns three decisions the protocol does not make:

1. **Who may call it.** The protocol carries no identity beyond the optional
   bearer token; the transport decides whose credentials these are.
2. **What `get` returns.** Real value, or a scoped stand-in.
3. **How a request reaches a human.** The protocol only says a request has an
   id and a status that eventually settles.

In Discobox: sandbox-agent serves the protocol on sandbox loopback, relays to
pool-agent over the sandbox's mTLS client certificate (which is the identity),
and pool-agent mints the ephemeral sentinel and calls the control plane, where
grants and cleartext live. See
[`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md) and
[`sandbox-agent/DESIGN.md`](../sandbox-agent/DESIGN.md).
