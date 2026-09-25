# 26-09-25-122 — A token may expire, and suggest the command that renews it

- **Status**: Accepted
- **Date**: 2026-09-25
- **Relates to**: [0011](0011-oauth-secrets-refresh-server-side-on-resolve.md),
  whose "serve what is on hand" this keeps while moving the renewal to a person's
  client; [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md),
  whose agents gain no way to name a command;
  [0130](0130-an-audit-record-is-read-where-it-was-written-and-names-its-attestor.md),
  whose trails gain one; [0131](0131-the-launcher-answers-every-servers-credential-requests-and-names-the-server-its-config-screens-edit.md),
  whose inbox carries the new ask; and
  [0132](0132-a-credential-rejected-after-its-retry-is-recorded-against-its-secret.md),
  whose rejection of a renewable token becomes an ask for a new value.

## Context

A `token` secret is typed in once and served until somebody replaces it. Many
of the credentials people actually hold are short-lived tokens that a command on
their own machine can produce at any time: `gh auth token`, `gcloud auth
print-access-token`. Such a command reads a keychain, refreshes a login, or
performs an exchange. Today, giving one of these to a discobox means running the
command by hand and pasting its output into a secret. When the value expires,
nothing notices: the discobox gets refusals, and a person has to work out why
and paste again.

What is missing is not a new kind of value. It is three facts about an existing
one:

- **A token can go stale.** The control plane needs to know when a value it
  holds has stopped being trustworthy.
- **Staleness is somebody's to fix.** A stale value needs a new one, and only
  a person's client can produce it, because the command, its login, and its
  keychain are on the person's machine. The server cannot run it, and neither
  can the pool.
- **How to fix it is known in advance.** The person who stored the token knows
  which command made it, and can say so.

Two constraints shape the rest. The proxy resolves inline on the request path
under a 10s timeout, so it cannot wait for a person. The server has no way to
reach a client: an attach is a websocket forwarded byte for byte, and a client
learns what the server wants by polling the approval inbox, which the launcher
already does every 5s (ADR 0131 §1).

## Decision

**A `token` gains an optional lifetime and an optional refresh command. Delivery
never waits: the proxy is always served the value on hand. Renewal is a separate
loop. When a value in use is stale or about to be, the control plane opens a
refresh ask in the credential inbox. Any person's client may answer it by
writing a new value. The command is advice to that client about how to produce
the value; the server never runs it and never checks that it was used.**

### 1. A token may expire, and may carry a refresh command

A `token` secret gains three fields:

- `ttlSeconds`: how long a value is good for after it is written. When it is
  unset, the token never goes stale, as today.
- `refreshCommand`: an argument vector a client may run to produce a new
  value. It is run directly, never through a shell. The server stores it and
  shows it, but never runs it.
- `valueUpdatedAt`: when the current value was written. It is set by every
  value write.

The value is stale at `valueUpdatedAt + ttlSeconds`. A write may instead carry
an explicit `expiresAt` for a value that knows its own lifetime.
`refreshCommand` without a TTL is allowed and defaults the TTL to 300s, because
a value worth fetching by command is short-lived by assumption.

The value itself is unchanged: `SecretValue.Token`, sealed as today, and
emitted alone by the resolve handler. The sentinel, the swap, and the grants do
not change. No new type is added, because the value is a token by every test
the system applies: the proxy swaps it as one, an ask for a token accepts it,
and it cannot renew itself.

**Only a person writes a refresh command.** It is a secret write, and the
sandbox role (ADR 0140 §4, `sandboxRole`) lists no secret write. This ADR adds
none. The agent credentials protocol carries no type or command (ADR 0031). A
discobox can ask for a credential and can answer an ask with a secret that
already exists. It can never name a command that a person's client will be
offered to run.

### 2. A well-known credential suggests a command

`wellknown.Credential` gains `RefreshCommand`, the command that produces the
credential on a machine that is logged in. `com.github.api` gets `gh auth
token`. A gate has none (ADR 0140).

Wherever a person gives a secret for a well-known ID, they get two choices:

- **Get it from a command**, with the ID's command filled in and editable. The
  client runs it right then to get the first value, and saves the secret with
  that value, the command, and the default TTL.
- **Enter a value**, which saves a plain token as today, with no lifetime.

The places this applies are the credential dialog that answers an ask by ID,
F4's new secret, and `discobox secret create --well-known <id>`.
`discobox secret create --refresh-command '<argv>' [--ttl <d>]` does the same
for a credential the registry does not know. Because the first value is fetched
when the secret is created, a secret with a command always starts with a value.
The registry never runs anything: what runs is always what the person saw in
the field.

### 3. Delivery serves what is on hand

`ResolveSandboxSecret` does not change what it returns: the stored value, under
a live grant. For a token with a lifetime, it also:

- **Caps the resolution** at the earlier of the grant's expiry and the value's
  stale time, as ADR 0011 §3 caps an OAuth resolution by its token. The proxy
  therefore re-resolves when the value ages out, and picks up a renewed value
  then.
- **Serves a stale value anyway**, with a 30s resolution. A stale value is a
  guess, not a known failure; the upstream is the authority on it (ADR 0011
  §4). The short resolution is so that the proxy picks up a renewal soon after
  it is written.
- **Opens a refresh ask** when the value is stale or within 60s of it, unless
  one is already open for the secret. Tying the ask to resolve means an idle
  secret is never refreshed, the same reason ADR 0011 rejected a background
  refresher.

A rejection of a token that has a lifetime or a command (ADR 0132) marks the
value stale and opens a refresh ask. It is not recorded as `unrefreshable`,
because it can be renewed; it is renewed by a person instead of by the server.
A token with neither is judged as today.

### 4. A refresh ask is a credential request, answered by writing the value

A refresh ask is a `SecretRequest` of a third species, beside reactive and
protocol-originated (see the secrets DESIGN). It has `reason: refresh` and
carries:

- the secret,
- the sandbox whose resolve or rejection opened it,
- why it was opened: `stale` or `rejected`.

It sits in the same table and appears in the same inbox, with the same `!` on
the list and the same workspace banner. Open refresh asks are deduplicated to
one per secret, because staleness belongs to the secret and not to the discobox
that noticed it. An ask that is still open when the value is next resolved
records the latest sandbox, so the banner follows the work.

**It is answered by writing a value, not by approving.** `POST
/secrets/{id}/refresh` carries:

- the new value,
- optionally an `expiresAt`,
- the ask it answers,
- how the client produced the value: the digest of the command it ran, or
  `entered`,
- whether a session permission answered it without a prompt.

The write goes through `store.UpdateSecret`, the point every value write
already passes through (ADR 0132 clears rejections there). It sets
`valueUpdatedAt` and closes every open refresh ask on the secret. An ordinary
`UpdateSecret` of the value closes them too, so any API client, a script, or a
person pasting a value renews the token just as well. The command is advice.
The server records how a value was made (§6), but it cannot tell a command's
output from a pasted value and does not try.

**The first answer wins.** A refresh naming an ask that is already closed is
refused with a conflict and writes nothing. Two clients that both saw the ask
then leave one value behind instead of two back to back. A refresh that names
no ask is an ordinary value write.

`approve` on a refresh ask is refused, because it has no grant to mint. `deny`
dismisses it, and the stale value keeps being served until the next resolve
opens another. The sandbox role may approve and deny credential requests
(ADR 0140 §4), but a refresh ask is refused to it on both routes. A discobox
supplies no value and makes no decision about when one is renewed. The refresh
route is not in the role.

### 5. The launcher offers to run the command; nothing else does

The launcher shows a refresh ask wherever it shows a credential ask (ADR 0131
§1). The dialog names the secret, why it was opened, the discobox that opened
it, and the command. It offers:

- **Run once.** The launcher runs the command and posts its output.
- **Run for this session.** The same, and the launcher remembers the secret and
  command digest. It then answers that pair's next asks as they appear, with no
  prompt, for as long as the window runs. The permission is kept in the
  window's memory and keyed by server, secret, and digest, so an edited command
  asks again.
- **Enter a value**, which uses the same field as F4.
- **Dismiss**, which denies the ask.

**How the command runs.** It is run with the argument vector as stored, no
stdin, the client's own environment, a 30s timeout, and stdout capped at 64KiB.
Stdout is trimmed, and an empty result is a failure. A failure posts nothing:
the ask stays open, the stale value keeps being served, and the command's
first line of stderr is shown in the dialog. A session permission that fails
falls back to prompting.

`discobox secret request list` shows refresh asks. `discobox secret refresh
<secret> [--run | --value -]` answers one from a shell, running the stored
command locally or reading a value.

**A raw attach does nothing.** It neither shows nor answers refresh asks. A
person working over `--raw` answers them as they answer credential asks today:
from the launcher or the CLI in another terminal.

### 6. Every renewal is on the record

Refresh asks are kept after they close, like other credential requests, and
each answer is recorded on its ask:

- who opened it: the sandbox, and whether a stale resolve or a rejection did,
- when it was answered, and by which principal and client host,
- how the value was produced: a command digest and argument vector, or
  `entered`,
- whether a session permission answered it,
- or that it was dismissed, or went unanswered.

The value is never recorded, not even as a hash. The trail is served to
`admin audit` as `refresh`. As ADR 0130 §2 splits verdicts, the ask, the
answer's arrival, and its principal are attested by `control-plane`. How the
value was produced is the client's own account, a new attestor `client` that
nothing on the server can verify.

## Alternatives rejected

**A new `exec` secret type whose value the server obtains by having an attached
client run its command.** This was the first draft of this ADR. It carried a
command digest that a fulfill had to match, a per-sandbox ask, a raw attach
that polled for approved asks and ran them, and an approval scope stored on the
server. Rejected because every part of it was machinery for making "the value
came from this command" true, and the server cannot know that anyway: a client
is trusted to post a value, and it could post any value. Once that is admitted,
the command is advice, the value is a token, and renewal is the same act as a
person pasting one. What remains is only what is new: a lifetime, an ask when
the lifetime runs out, and a suggestion for answering it.

**Hold the resolve open until a client renews.** Rejected because the proxy's
resolve times out at 10s, clients learn of the ask on a 5s poll, and a person
may take a minute to answer. Delivery and renewal are separate loops, and
coupling them would fail requests that the value on hand serves perfectly well.

**Refuse a stale value instead of serving it.** Rejected because a TTL is the
person's estimate, not the upstream's verdict. Serving the value lets the
upstream decide. A value that really has expired is refused there, and that
refusal opens the same ask (§3).

**Track attached clients, and ask only when one is present.** Rejected because
an unanswered ask costs nothing, and a registry of attaches would add
heartbeats and failure modes to decide something the inbox already decides. It
would also exclude a person in the launcher who is not attached to the discobox
that needs the value.

**Run the command from a raw attach.** Rejected because a raw attach has
nowhere to ask a person, and running a command no person was asked about
defeats the point of offering it in a dialog.

**Store the session permission on the server.** Rejected because it is
consent to run something on one machine for as long as a window runs. Only the
client knows when that ends, and a flag on the server would outlive the window.

**Keep commands in client configuration only.** Rejected because §1 already
ensures that no discobox can write a command. A person's command stored on the
secret is also what lets any of their clients answer, not only the machine
where it was first configured.

## Deferred

**Kubeconfig secrets.** A kubeconfig names a cluster, its CA, and a user who
authenticates with a bearer token, a client certificate, or an exec plugin.
Delivering one needs secret delivery as a file and host trust derived from a
secret, and neither exists yet. A client certificate needs more still: the
proxy presents none upstream, so it would first have to become a
client-certificate secret usable for mTLS. An exec-plugin kubeconfig fits this
ADR as a token with a refresh command, once a client can read `ExecCredential`
JSON for the value and its `expirationTimestamp`. Revisit when a kubeconfig is
the credential somebody is waiting on.

**A command that reports its own expiry.** Only plain stdout is read, so the
TTL governs. `ExecCredential` output is how a command would say otherwise, and
it lands with kubeconfig.

## Consequences

- `SecretValue`, the `Secret` API model, and `CreateSecretBody` gain
  `ttlSeconds`, `refreshCommand`, `valueUpdatedAt`, and a read-only `staleAt`.
  `marshalSecretValue` maps them, and a migration backfills `valueUpdatedAt`
  from `updated_at`. The secret type enums do not change.
- `SecretRequest` gains `reason: refresh` and `refreshCause`, deduplicated per
  secret. `ApproveSecretRequest` refuses one, and the sandbox role is refused
  both approve and deny on one. `ResolveSandboxSecret` gains the resolution cap
  and the ask, and `judgeRejection` gains the renewable-token case.
- `POST /secrets/{id}/refresh` is new, and is not in the sandbox role.
  `store.UpdateSecret` closes open refresh asks and sets `valueUpdatedAt`.
- `wellknown.Credential` gains `RefreshCommand`. The credential dialog, F4, and
  `discobox secret create` gain the command-or-value choice.
- The launcher gains the refresh dialog and a per-window set of session
  permissions. `discobox secret refresh` is new. A raw attach does not change.
- `admin audit` gains the `refresh` trail and the `client` attestor.
- A value is served up to one TTL, plus however long a person takes to answer,
  past its estimate. The upstream decides whether that is too old, and a
  refusal comes back as the same ask.
