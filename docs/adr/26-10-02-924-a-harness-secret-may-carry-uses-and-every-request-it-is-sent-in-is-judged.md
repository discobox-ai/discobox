# 26-10-02-924 — A harness secret may carry uses, and every request it is sent in is judged

- **Status**: Proposed
- **Date**: 2026-10-02
- **Depends on**: [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §1, §2, §4 and §6, which are built. The project judge, request
  authorization, rounds and verdict records are what this extends.
  [26-10-02-054](26-10-02-054-commands-are-judged-by-default-and-requests-by-opt-in.md)'s
  request switch decides whether it applies, and
  [26-10-02-393](26-10-02-393-a-credential-request-and-its-grant-may-name-several-hosts.md)'s
  host lists are what the grant carries.

## Context

A harness's credential is collected by its configure flow and bound to the
harness config with a standing, config-scoped grant
(`applyConfigureOutput`). That grant has no uses. Its sentinel is injected
into every sandbox on the config, and it never gets an activation. So the
pool's `secretResolver.authorize` finds no use IDs for it and allows the
request without asking the judge. `ResolveSandboxSecret` then swaps it for
the host and grant policy alone (ADR 838: "A static harness credential with no
activation has no approved-use sentence and keeps its existing host and grant
policy").

That was enough while every harness credential opened only its own model API.
GitHub Copilot's is different
([ADR 26-10-02-840](26-10-02-840-the-copilot-harness-takes-its-github-token-from-the-environment.md)).
A `/login` token is a GitHub OAuth App token with `repo` scope. Copilot needs
it at `api.github.com` (`/copilot_internal/user`) and under
`githubcopilot.com`, including the GitHub MCP server, which acts on
repositories with that same token. No host covers what Copilot needs without
also covering the account's repositories: a grant may now list several hosts
(ADR 26-10-02-393), but one of the two Copilot needs, `api.github.com`, is
the whole REST API. Every request the sentinel is sent
in is either something Copilot does to run or something the agent is doing
with the user's GitHub account, and only reading the request tells the two
apart.

That is what the request judge does for agent credentials. Host trusts
([ADR 0149](0149-a-host-certificate-is-trusted-for-one-sandbox-when-a-person-pins-it.md)
§5) already judge every request to a pinned host against uses that live on
something other than a grant, with no command behind them. And a fast
decision model makes it affordable to judge every request in the API tier.

## Decision

### 1. A harness image may declare uses on a secret

`harness.Secret` gains `uses`: sentences, written the way an approved use is
— as what the harness itself sends. They are part of the image manifest,
snapshotted onto the harness config with the rest of it and re-snapshotted
when the image moves. A configure command's output cannot add to them or
change them: the image knows what its CLI sends, and the configure flow runs
inside a sandbox. For Copilot, one use:

> GitHub Copilot CLI's own use of this token: Copilot AI requests (its
> entitlement, policy and settings checks under api.github.com/copilot_internal,
> listing models, chat and completions, and telemetry under githubcopilot.com)
> and light read-only GitHub queries, such as reading a file or searching code;
> nothing that creates, changes or deletes anything on GitHub

Measured with `test/judge-evals/cases/copilot-*` (2026-10-08, prompt version
8, the claude-code judge, 3 runs each, each job's host the destination itself
as §4 says a grant with no hosts reports it): 48 of 48 runs right. Every request
Copilot 1.0.91 was seen sending was allowed, and every control was refused:
a REST write, a pull request, deleting `main`, adding a collaborator, a gist,
a workflow dispatch, a write through the full MCP endpoint, and a GraphQL
mutation. A read-only MCP call is asked about in round one and allowed once
its body is shown.

A secret with no uses keeps today's behavior. Declaring uses is how an image
says its credential reaches more than the harness needs.

### 2. The configure-created grant carries them

When configure output is applied, and whenever the snapshot changes, the
config-scoped grant for that secret is minted with the declared uses. The
server mints the use IDs. The grant gets no `EnvName`, so it never shows up
in `discobox-access list`, and it never gets the lazy agent binding that a
grant with uses otherwise does. A harness credential is not one the agent
asked for.

### 3. The pool judges a judged harness sentinel without an activation

The sentinels pushed to a pool say which ones belong to a grant with uses, and
which use IDs those are, the way a host trust's `TrustUseIDs` already reach
the proxy. `secretResolver.uses` includes them for any request carrying such a
sentinel, activation or not. The request is allowed if any of the uses allows
it. A refusal is a 403 from the proxy, audited once as blocked, as today.

### 4. The request job is framed as the harness's own traffic

The job has no command. Its `Credential` names the harness and the secret, and
its `Purpose` is the use. Its `Host` is the grant host that covers the
destination, as ADR 26-10-02-393 §2 reports an approved use; for a grant with
no hosts — a harness secret may deliberately have none — it is the destination
the proxy observed, since `Job.Validate` requires one. The judge is told this credential belongs to the
harness running in the discobox and not to the agent. Requests that serve the
agent's own aims, like pushing a branch or opening a pull request, are outside
these uses however the agent phrases them.

### 5. It follows the request switch, and the judge's own harness is exempt

Requests are judged only where the server judges requests
(`judgeCredentials`, ADR 26-10-02-054), by whichever backend judges them —
the judge discobox or Jev. There, a judged harness sentinel is never swapped
unjudged: a judge that cannot answer refuses the request, and `FindLiveGrant`
must not hand out the value of a grant with uses for a request the pool did
not have judged (today it does not check). Where requests are not judged, the
uses go unenforced and the swap is held to the grant's hosts alone, as every
request there is — the operator's choice, as ADR 054 §3 says of commands — and
the harness config reports that its secret's uses are not being enforced.

The judge discobox's own harness credential is exempt by its sandbox mode,
explicitly in code. ADR 838 relies on that credential having no use. A judge
whose model calls were themselves judged would wait on itself.

### 6. Standing allows cover harness routes

ADR 428's standing allows apply unchanged, keyed by sandbox, use, origin and
route. A harness's repeated reads and its model calls to one route are judged
once per standing window. A request whose operation is in its body, such as an
MCP call, is judged every time, and that is where an agent would reach a
repository through Copilot. Under ADR 26-09-30-854, a harness request's argv
in that key is empty.

## Rejected alternatives

- **Path rules on the grant** (`GET /copilot_internal/*` on `api.github.com`,
  everything under `githubcopilot.com` except `/mcp`). These are exact and
  cost no model call. But they are tied to one CLI version's endpoint layout,
  which the vendor does not document, and they cannot tell a read-only MCP
  call from a write on the same route. They may come back as ADR 838 §7's
  endpoint rules in front of the judge, as an optimization.
- **Uses on the configure output.** Configure runs in a sandbox, and what the
  harness CLI sends is a property of the image, not of one configuration.
- **Exposing the grant as an agent credential with an `EnvName`.** That would
  give the agent the credential to `run` under its own uses. The point is the
  opposite.
- **Refusing the harness wherever requests are not judged.** A harness that
  declares uses says its credential is unsafe unjudged, but request judging is
  off by default (ADR 26-10-02-054), so this would make Copilot unusable on a
  default server. Reporting the gap instead leaves the choice where 054 put
  it, with the operator.

## Consequences

- Every request a judged harness sentinel is sent in costs a judge decision or
  a standing-allow lookup. Copilot's model traffic is mostly covered by
  standing allows. Its MCP calls are not.
- On a server that judges requests, a `/login` Copilot token is bounded by
  these uses, and the configure warning can say so once this is built. On one
  that does not, it is not, and the harness says so.
- `harness.Secret`, the manifest snapshot, `applyConfigureOutput`, the
  sentinel push to pools, `secretResolver.uses`, `FindLiveGrant`, the request
  job, and the judge's system prompt (and `PromptVersion`) change together.
- Judging the judge's own harness stays a carve-out. If judges ever run a
  harness whose secret declares uses, this needs revisiting.
