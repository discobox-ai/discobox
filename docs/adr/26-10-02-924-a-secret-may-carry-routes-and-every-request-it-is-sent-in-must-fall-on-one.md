# 26-10-02-924 — A secret may carry routes, and every request it is sent in must fall on one

- **Status**: Accepted (narrows [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §4: an injected credential whose secret carries routes is held to them,
  with no activation)
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
the whole REST API.

Judging every request the token is sent in against one sentence describing
all of Copilot's traffic was tried first. Real traffic showed the judge
refusing Copilot's own model calls: half of `POST /v1/messages` and most of
`/mcp/readonly` on `api.individual.githubcopilot.com`. Most of what Copilot
sends needs no judgment at all. Only a few routes reach the account's
repositories.

## Decision

### 1. A secret carries routes

`Secret` gains `routes`, beside `host`. A route is `{host, path?, use?}`:

- `host` covers itself and every host beneath it, as a grant's host does.
- `path`, when set, is a URL path prefix matched on whole `/` segments:
  `/mcp` covers `/mcp` and `/mcp/readonly`, not `/mcpx`. `/` is the whole
  host.
- `use`, when set, is a sentence written the way an approved use is. A route
  with one has every request on it judged against it. A route without one
  sends them unjudged.

A person sets routes at create or update (`--route HOST[/PATH][=USE]`). A
harness configure flow writes the ones its image declares onto the secret it
creates (§2). A secret with no routes is left to its grants, as today.

Use IDs are the control plane's, minted for a route with a use when the
routes are written. A route whose host, path and use do not change keeps its
ID, so a verdict and a standing allow recorded against it last. A change is
pushed to every running discobox holding the secret.

Route uses are nobody's to run under. They are not a grant's uses, so they
are not listed as uses an agent may ask for, and `ApprovedCredentialUse` does
not match them.

### 2. A harness image may declare routes on a secret

`harness.Secret` gains `routes`. They are part of the image manifest,
snapshotted onto the harness config with the rest of it and re-synced onto
the configured secret when the image moves. A configure command's output
cannot add to them or change them: the image knows what its CLI sends, and
the configure flow runs inside a sandbox. A secret a person bound to a
harness config keeps the routes that person gave it.

Copilot's token sends everything under `githubcopilot.com` unjudged, and so
`/v1/messages`, `/mcp/readonly` and `/agents/swe/internal/memory` named
explicitly. It also sends `api.github.com/copilot_internal` and
`api.github.com/repos/github/copilot-cli/releases` unjudged. It judges
`githubcopilot.com/mcp`, `githubcopilot.com/agents` and `github.com` against
read-only uses. The judged uses have not been re-measured with
`test/judge-evals`, which still holds the single sentence this replaced.

### 3. The most specific route decides

For a request to host H with path P, the pool considers the routes whose host
covers H and whose path covers P:

- The most specific wins: the host with the most labels first, then the
  longest path.
- Every route tied at that host and path forms a group. If any route in the
  group has no use, the request is allowed unjudged. Otherwise it is judged
  against the group's uses, and any one allowing is enough.
- If no route covers the request, it is refused without asking the judge:
  "this credential has no route to H/P". Routes are an allowlist.

So `githubcopilot.com/mcp/readonly` is allowed while the rest of
`githubcopilot.com/mcp` is judged, and `githubcopilot.com` covers the rest.

P is the request's path decoded with its dot segments resolved, so
`/mcp/readonly/../write` is `/mcp/write`, and it is matched without regard to
case, so `/MCP` cannot step around a judged `/mcp`. An encoded `/` is a
separator to an upstream that decodes the path and data to one that routes on
it as sent, and the proxy cannot tell which it is talking to, so a path that
holds one is also read the second way, with dot segments resolved around it.
The request is held to both readings: each must fall on a route, and each
judged one is a group. So `/mcp/x%2F..%2Freadonly` is judged under `/mcp`
however the upstream reads it.

The sentinels pushed to a pool carry their secret's routes
(`sentinelRoutes`), and the agent-credential listing carries them
(`secretRoutes`) for an activation to record. `secretResolver.uses` returns
groups: a grant's or trust's use is a group of one, and the secret's matching
route is another. Every group must allow. So an activation must have its
grant's use allow and fall on one of its secret's routes. A refusal is a 403
audited once as blocked.

### 4. The judge is asked as for any approved use

`ApprovedUse` finds a route's use among the secrets the discobox holds,
injected or agent-requested. The job is the one any approved use produces:
`Credential` is the secret's name, `Purpose` the route's use, `Host` the
route's host. The verdict is recorded against the grant it was handed out
under, or none for an injected secret. The judge, its prompt and its job do
not change.

### 5. It follows the request switch, and the judge's own credentials are exempt

Requests are judged only where the server judges requests
(`judgeCredentials`, ADR 26-10-02-054), as every approved use is. Where they
are not, judged routes go unenforced. A request no route covers is still
refused, because that needs no judge.

A judge discobox's injected secrets carry no routes to the pool, by its
sandbox mode. ADR 838 relies on that credential having no use, and a judge
whose model calls were themselves judged would wait on itself.

### 6. Standing allows cover judged routes

ADR 428's standing allows apply unchanged, keyed by sandbox, use, origin and
route. A request whose operation is in its body, such as an MCP call, is
judged every time, and that is where an agent would reach a repository
through Copilot. Under ADR 26-09-30-854, a harness request's argv in that key
is empty.

## Rejected alternatives

- **Uses only, every request judged.** This was the first version. The judge
  refused Copilot's own model traffic. It also paid a model call for traffic
  that never reaches anything a person owns.
- **Per-host uses only.** A use per host cannot exempt `/mcp/readonly` while
  judging the rest of `/mcp` on the same host.
- **Unmatched requests sent unjudged.** That would leave the credential free
  on any host nobody named, which is the gap this closes.
- **Matching on method.** Deferred. Revisit if a route needs to be read-only
  by method, such as `GET` only on a REST prefix.
- **Path rules on the grant.** A grant is the agent's, but the routes describe
  where the secret goes whoever holds it, including a harness that holds no
  grant of its own.
- **Routes on the configure output.** Configure runs in a sandbox, and what
  the harness CLI sends is a property of the image, not of one configuration.
- **Exposing the grant as an agent credential with an `EnvName`.** That would
  give the agent the credential to `run` under its own uses. The point is the
  opposite.
- **Every route in a tied group having to allow.** Two routes for the same
  place say it may be used for either. Requiring both would make the second
  narrow the first.
- **Refusing the harness wherever requests are not judged.** Request judging
  is off by default (ADR 26-10-02-054), so this would make Copilot unusable on
  a default server. Leaving judged routes unenforced there leaves the choice
  with the operator, as ADR 054 does.

## Consequences

- Copilot's model traffic costs no judge call. Only its MCP and agent calls,
  and GitHub beyond Copilot's own endpoints, are judged.
- A harness route list is tied to the endpoint layout of one CLI version,
  which the vendor does not document. A new endpoint is refused until the
  image names it. That fails closed, and the refusal names the host and path
  to add.
- On a server that judges requests, a `/login` Copilot token is bounded by
  these routes, and an agent needs a grant of its own for anything else on
  GitHub. On a server that does not, judged routes are not enforced, but the
  token still reaches nothing off its routes.
- `harness.Secret`, the manifest snapshot, `Secret`, its API, CLI and TUI,
  the configure flow, the approved-use lookup, the sentinel push and agent
  listing to pools, and `secretResolver.uses` change. The judge does not.
  `secretroute` holds the matching, shared by the control plane, the pool and
  the CLI.
- Judging the judge's own credentials stays a carve-out. If a judge ever
  holds a secret with routes, this needs revisiting.
