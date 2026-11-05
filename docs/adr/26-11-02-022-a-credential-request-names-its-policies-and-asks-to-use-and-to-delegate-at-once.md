# 26-11-02-022 — A credential request names its policies, and asks to use and to delegate at once

- **Status**: Accepted (§3 amended before implementation landed: the summary is computed by whatever shows the role, from the routes the server resolved it to and the request carries; §5 amended for a use part approved to add by [26-11-03-329](26-11-03-329-a-box-may-be-granted-routes-for-a-time-beside-its-binding-without-replacing-it.md); §§3–4 and 6 amended for approval scope and historical consent by [26-11-04-616](26-11-04-616-whole-credential-is-an-explicit-approval-scope.md))
- **Date**: 2026-11-02
- **Amends**:
  - [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md):
    - §1's `SecretRequest`, whose `Use` and `Delegate` share one `Routes`.
      Each becomes a part with its own routes (§1).
    - §2, for what an approval writes. An approved part leaves the box one
      sandbox-level binding of that kind for the secret (§5). A person may
      still write others.
    - §3's "a request that asks for `Delegate` is answered by a person
      only". It covers the whole of a request that has a delegate part,
      including its use part and its denial (§4).
  - [26-10-30-584](26-10-30-584-a-credential-policy-file-supplies-a-bindings-routes.md)
    §2's "the CLI reads and validates the file … the server receives routes".
    That applies to bindings only. A request sends the documents, and the
    server resolves them (§2). §§1, 3, 4 and 6 apply unchanged to a
    request's policies.
- **Leaves standing**: 880's Deferred redesign of the discobox-access
  protocol. This ADR changes what a request carries and how it is answered.
  It does not decide how a credential is delivered or activated, and it is
  not that redesign.

## Context

A box that creates workers (a manager, ADR 26-09-30-782) needs two kinds of
access to one secret. It uses the secret itself, and it delegates the secret
to its workers. `discobox-access request` takes one `purpose` per request, so
the box files two requests and a person approves two screens. Each request
spells out raw routes, even when the routes already exist as credential
policy files (ADR 26-10-30-584). discobox-ai/manager's delegation is 35
GitHub routes (three roles across four repositories), and its own access is
another 31. A script, `roles/request.py`, generates them from its
`roles/*.json` policy files only because `discobox-access` cannot read those
files.

Two things are unsettled today:

- **Whether a use binding and a delegate binding of one secret coexist.**
  Approval (`putRequestedBinding`) extends a standing binding of the same
  kind instead of adding one. It matches a use binding by **variable** and a
  delegate binding by **secret**. A standing binding that carries both flags
  matches either way, so approving one kind rewrites the routes of the other.
  A person may also have given the box several use bindings of one secret,
  on different variables (880 §2 keys use bindings by variable).
- **What the approver reads.** It is every resolved route: 66 lines for the
  manager, in which nobody can see the roles they came from.

`discobox-access` may depend only on `agentcreds`, which uses only the
standard library (access/DESIGN.md). The resolver, `credentialpolicy`,
depends on `secretroute`, and through it on a GraphQL parser. The CLI can
read a policy file, but it cannot resolve one.

## Decision

### 1. A request has a use part, a delegate part, or both

Each part has its own route list. The use part also names the variable the
credential is delivered in. The delegate part names no variable, as a
delegation today does not. A request has at least one part.

```sh
discobox-access request --name github --env-var GH_TOKEN \
  --policy roles/github-submit.json --policy-arg owner=discobox-ai --policy-arg repo=discobox \
  --delegate-policy roles/github-submit.json --policy-arg owner=discobox-ai --policy-arg repo=vm \
  --delegate-policy roles/github-review.json --policy-arg owner=discobox-ai --policy-arg repo=vm \
  --justification "…" --ttl 72h --wait
```

- **Use part:** `--route`, `--hosts` or `--policy`, plus `--env-var`.
- **Delegate part:** `--delegate-route` or `--delegate-policy`.
- **`--delegate` and `purpose` go.** The credentials service, not only the
  CLI, refuses unknown fields as `invalid` and names the field. A box
  running an older `discobox-access` that still sends `purpose` is told so,
  instead of having the field silently dropped and failing on the variable.

The `--json` form carries the same request:

```json
{
  "name": "github",
  "envVar": "GH_TOKEN",
  "policies": [
    { "file": "roles/github-submit.json", "args": { "owner": "discobox-ai", "repo": "discobox" } }
  ],
  "delegate": {
    "routes": ["GET api.github.com/repos/discobox-ai/vm/**"]
  },
  "justification": "…",
  "grantTTLSeconds": 259200,
  "wait": true
}
```

The top-level `routes`, `hosts` and `policies` fields are the use part. The
`delegate` object, with its own `routes` or `policies`, is the delegate
part. A policy is named by `file`, and its arguments are an object.
`discobox-access` reads the file and sends its document with it (§2). One
lifetime is asked for, and it applies to both parts.

### 2. Policies travel as documents, and the server resolves them

`--policy FILE` and `--delegate-policy FILE` may each be repeated. Every
`--policy-arg NAME=VALUE` belongs to the nearest `--policy` or
`--delegate-policy` before it, the scoping rule `secret bind` uses (ADR 584
§4). An argument before any policy flag is a usage error. On each part's
list, policies and inline routes are mutually exclusive (ADR 584 §2).

`discobox-access` reads each file relative to its working directory. It
refuses, naming the file, one that is not a single JSON object, that
repeats a key at any depth, or that exceeds 64 KiB. It refuses a `--json`
body that repeats a key the same way, so a duplicated argument is never
reduced to its last value. The token-level check uses only the standard
library. The CLI otherwise leaves the document alone. Every hop to the
server (the credentials service, the pool, the server's API) carries it as
a JSON object, never as a quoted string.

The server validates and resolves each document with `credentialpolicy`
under ADR 584 §§3, 4 and 6, and an invalid one refuses the whole request as
`invalid`, naming the file and the field. The box controls every hop's
input, so the client-side checks are for the agent's sake and are not the
boundary. The boundary is that the server parses each document once, and
both the routes it stores and everything it shows come from that parse
(§3). `credentialpolicy` moves from `cli/internal` to `pkg/` so that the
CLI and the server share one resolver.

Limits:

- **A request body** may be up to 1 MiB (`agentcreds.MaxBodyBytes`, at
  64 KiB today, rises for it).
- **A request** names at most 32 policies across its parts.
- **A part** resolves to at most 512 routes.
- The server refuses a request over any of these, naming the limit. ADR
  584's per-policy expansion bound still applies to each document.

The server keeps the resolved routes on the request, one list per part. It
does not keep the documents.

### 3. The approver sees roles, with what they grant beside them

For each policy, the request records:

- the file's base name without `.json`;
- its `description`;
- its arguments;
- a digest of its document.

Policies are grouped by digest and arguments, never by name, and shown part
by part: "github-submit — Open pull requests in one repository:
discobox-ai/discobox, vm, manager, review". When two documents with
different digests share a name, both are shown and marked as different.

The name and description are text the box wrote. They grant nothing, and
the approver is told they are the box's description. They are rendered as
plain text, as a justification is. So the label is never shown alone.
Beside each role, whatever shows it shows a summary computed from the routes
the server resolved that role to, which the request carries per role, and
never from anything the box wrote:

- the hosts;
- the methods, or "any method";
- the path patterns, with every wildcard (`*`, `**`) marked, or "any path";
- the GraphQL operations and MCP tools it allows.

On a REST API the path is the scope, so a role that resolves to `POST
api.github.com/**` does not get the same summary as one resolving to `POST
api.github.com/repos/discobox-ai/vm/pulls`. A role given an argument that
none of its routes uses is marked, so the arguments shown beside it never
suggest a narrowing the document does not make. The full route list is one
expansion away in the console and in `discobox secret request get`. A label
that claims "read" or "one repository" over a summary that says `POST`,
`mutation` or `/**` is visible as such on the first screen. Inline routes are shown as routes, as
today.

As with bindings (ADR 584 §5), a binding stores only routes. An approver may
narrow or replace a part's routes when approving. A part approved with
routes other than those its policies resolved to is shown as edited, so a
role name is never displayed over routes it no longer describes.

### 4. One answer holds two decisions

A person answers the whole request at once. For each part they either:

- **approve it**, with the routes and lifetime as asked or narrowed, or
- **deny it.**

The approve call names a decision for **every** part the request has. The
server refuses one that leaves a part out, so a grant to delegate is never
made by omission. `discobox secret request approve` therefore takes per-part
flags whenever the request has both parts, and the console shows both
decisions on one screen. The deny call denies every part. Both parts are
written in one transaction, so a request is never half-answered. Denying one
part leaves the other's approval standing. The raw value
(`ApproveSecretRequestBody.raw`, ADR 26-10-27-840) belongs to the use part
only.

The request records each part's outcome and the binding the part made. The
request is `approved` when any part was approved, and `denied` when none
was.

`discobox-access` reports each part (`agentcreds.RequestStatus` gains a
per-part outcome). A `--wait` exits 0 only when every part asked for was
approved. When any part was denied, it exits non-zero, since a shell-driven
agent reads zero as "I have what I asked for". The approved part is still
delivered. `agentcreds.Credential` and `discobox-access list` are unchanged:
a held binding is still either a use or a delegation.

A discobox answering its own discoboxes' requests (880 §3) answers only
requests that have no delegate part. It may list and read one that has a
delegate part, but approving or denying it is a person's. One answer is
never split between two answerers.

An identical pending ask is still reused rather than filed again. Identical
now means the same box, the same credential name, the same variable (or
none, for a request with no use part), the same lifetime, and the same set
of parts with the same resolved routes in each. A role's name, description and arguments, like the
justification, are left out, since they do not change what approval grants.

### 5. An approved part replaces the box's bindings of that kind

A request states the whole access the box wants of that kind. It never adds
to what the box already holds. When a part is approved, it replaces the
box's own sandbox-level bindings of that kind for that secret:

- **Use:** every sandbox-level `Use` binding of the secret, whichever
  variables they deliver it in, becomes the one binding the approval
  describes.
  - One standing use-only binding is updated in place: the one already in
    the asked variable, or else the oldest.
  - The other use-only bindings are deleted, with their sentinels.
  - A combined binding among them loses only its `Use` side, as in a split
    below, and is never deleted. Its delegation, and everything handed on
    under it, stays, even when the request had no delegate part or that
    part was denied.
  - A variable that already holds a *different* secret is still refused
    instead of taken over.
- **Delegate:** the sandbox-level `Delegate` binding of the secret is
  updated in place. It keeps its ID, so the bindings handed on under it stay
  and are held to its new routes from then on (880 §3). Narrowing a
  delegation narrows every worker under it.

Bindings at the harness config and project levels are not the box's own
(880 §2) and are not touched. A denied part changes nothing. Before
approving, the approver is shown what each part would replace: the standing
bindings of that kind, with their variables and routes, and which of them
would go. Access lost to a replacement is therefore on the screen, not a
surprise.

A standing binding that carries both flags, which a person can write, is
split when either part is approved:

- The standing binding keeps its ID as the `Delegate` binding, so what was
  handed on under it keeps its parent.
- A new `Use` binding takes its variable.
- The binding's `sandbox_secrets` row moves to the new binding with the
  same sentinel, so commands already holding it keep working.
- The approved part's routes and lifetime go to its own kind, and the other
  kind keeps the ones it had.
- The split writes an `updated` event for the standing binding and a
  `created` event for the new one, both with the request as their cause.

A combined binding that holds the raw value is not split by an approval. As
today, the approval is refused (409) until a person changes that binding.

All of this, including the sentinel changes, happens in the approval's
transaction. After a part is approved, the box holds at most one
sandbox-level binding of that part's kind for the secret. Once both kinds
have been approved, it holds at most one `Use` and one `Delegate`, each with
its own routes. The
two lists are independent. A `Delegate` binding has no variable and gets no
sentinel, so delegating a route never lets the box send on it. A `Use`
binding is never a parent, so using a route never lets the box hand it on.

The other existing refusals stand, and they apply to every binding a
replacement would update, split or delete, not only to the one it updates:

- A raw use binding is renewed or removed only by a raw grant (ADR
  26-10-27-840).
- A binding held under another parent is not taken over. A discobox
  approving under its delegation (880 §3) therefore never updates or
  deletes a binding a person wrote, or one held under another delegation.

Any such binding refuses the whole approval (409), naming it, and nothing
is written.

### 6. The protocol, the record and the upgrade

- **`agentcreds.RequestBody`:** drops `Purpose`, and gains `Policies` and
  a `Delegate` part.
- **`agentcreds.RequestStatus`:** reports each part's outcome.
- **`SecretRequest`:** drops `purpose`. Beside the use part's existing
  columns (variable, routes, binding ID), it gains:
  - the use part's policies, for display, and its outcome;
  - a delegate part: routes, policies for display, outcome and binding ID.

The upgrade is a migration that converts the existing rows in place:

- **A row with `purpose: delegate`** becomes a request with only a delegate
  part. Its routes and binding ID move to that part.
- **Any other row** becomes a request with only a use part.
- **Each part's outcome:**
  - for an approved row, `approved`;
  - for a denied row, `denied`;
  - for a pending row, none yet, so it is answered with one decision for
    its one part.
- **Bindings** are not rewritten. A combined binding stays as it is until an
  approval splits it (§5).

`secret bind` and `discobox run --bind` are unchanged. A binding a person
writes is still resolved by the CLI and still sends routes (ADR 584 §2).

## Alternatives rejected

- **Resolve the files in `discobox-access`.** That would give the module a
  dependency beyond `agentcreds`, through `secretroute` and its GraphQL
  parser, and the module is meant to be liftable on its own.
- **Resolve them in the sandbox's credentials service and send routes and
  labels.** The routes, and the summaries computed from them, would come
  from the box's resolution and not the server's. Resolving on the server
  keeps the routes stored and the summary shown from one parse that the
  server did. The label is the box's under either choice, which is why it
  is never shown without the summary (§3).
- **Send a path for the server to open.** ADR 584's reason stands: there is
  no server-side file lookup, and the files live in the box.
- **Send the document as a quoted string, so every hop keeps its bytes.**
  A hostile box controls those bytes anyway. Duplicate keys matter only
  when two parses could disagree, and the server parses once.
- **Two requests linked as a group.** That still means two answers, two
  rows to keep in step, and a wait that has to watch both. One request
  with two parts is what the person reads, and what they answer.
- **One route list with both flags** (880 §2's binding shape, used for a
  request). Use and delegation could not differ, so a manager delegating
  its workers' 35 routes would also be allowed to send on all of them.
- **An approval adds to the standing binding.** The binding would grow
  silently with each ask, and a box could never narrow itself by asking
  again. A request that states the whole access is the one a person can
  read.
- **Replace only the use binding in the asked variable** (today's match).
  The box's other use bindings of the secret would survive the approval,
  and the request would not state the box's whole use of it.
- **Approve a part the answer leaves out as asked.** Approving only the
  use part, as an approve call reads today, would also grant the right to
  delegate, which nobody looked at.
- **Let a discobox answer the use part of a request with a delegate part.**
  The person would face half a request, and its outcome would depend on who
  answered first.

## Consequences

- discobox-ai/manager files one request naming its role files and drops
  `roles/request.py`. The person sees its roles, and what each grants, on
  one screen, and answers them there.
- Re-asking is how a box changes its own access, wider or narrower. A
  person approves each change and sees what it replaces.
- A box's skills that ask with `--delegate` or `"purpose": "delegate"` are
  refused at once. The implementation changes with them:
  - the `discobox-access` skill;
  - the deliver and orchestrate skills that hold such examples;
  - `docs/agent-credentials-protocol.md`;
  - `access/DESIGN.md`.
- `credentialpolicy` becomes a shared package in `pkg/`, and its
  `REVIEW.md` notes on expansion bounds move with it. The server now
  resolves input from the box, not only a person's own CLI.
- These change too, and the migration converts existing rows:
  - the secret request record and its API;
  - the approve call;
  - the approval's transaction;
  - the console's request view.
