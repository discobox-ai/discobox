# Secrets Design

This package owns credentials: their storage, the approval lifecycle that
authorizes them, and the two ways a sandbox comes to use one.

Cleartext leaves the control plane through exactly one door — `ResolveSandboxSecret`,
called by a pool agent's proxy for one sentinel and one destination host. Every
other surface here deals in sentinels, requests, and grants.

## Grants authorize; requests are the inbox

A `SecretGrant` is the durable authorization: a secret, a scope (sandbox,
harness config, or project), a host, and an expiry. A `SecretRequest` is only a
question someone asked, and approving one mints a grant. Revoking the grant
takes the credential away even though the request row stays approved — the
request is history, the grant is the authority.

## Two species of request

They share a table and are handled apart on purpose (ADR 0031 §5). Declared uses
are what tells them apart (`SecretRequest.FromProtocol`).

| | Reactive | Protocol-originated |
| --- | --- | --- |
| Origin | The proxy hit an unresolvable sentinel | An agent asked, through the agent credentials protocol |
| Carries | type, host, sandbox | plus name, env var, justification, declared uses |
| Approval mints | a grant at the chosen scope | a sandbox-scoped, host-scoped grant with minted use IDs, and a stable binding |

Approving a protocol request is stricter than approving a reactive one, and the
strictness is refused rather than silently relaxed:

- **A concrete host is mandatory.** `FindLiveGrant` matches the destination the
  proxy actually observed, so the host is what stops a token being swapped
  toward somewhere it was not approved for. A wildcard grant stays an explicit
  administrative act via `discobox secret grant create`.
- **Sandbox scope only.** The agent asked on behalf of one sandbox; approving it
  project-wide would answer a question nobody asked.
- **Use IDs are always minted here.** A requester supplies descriptions, never
  IDs, so an agent cannot name the use it will later present. An approver may
  rewrite the descriptions; supplied IDs are dropped either way.

## Two ways to reach the agent credentials shape

A credential the sandbox cannot read — no environment variable, no
`secrets.json`, no sentinel in the proxy's match set — is a `SandboxSecret`
marked `AgentRequested` plus a grant carrying uses. Nothing else produces it,
and the two halves are what make it work: the flag keeps the credential out of
the sandbox, and the uses are what `discobox-access` names to take a value
(`FindLiveAgentGrant` matches only a sandbox-scoped grant that has some).

**The grant may be wider than the binding.** A grant carrying uses is matched
at any of the three scopes — the discobox, its harness config, or the project —
because the per-discobox part is only the binding, and that is minted lazily:
`ListLiveAgentCredentials` starts from the grants covering this discobox and
ensures each has a sentinel bound here, the first time that discobox's agent
asks what it may use. A grant on a project cannot bind in advance, since the
boxes it covers may not exist yet. Minting on a read path is deliberate; the
alternative is a reconciler chasing every discobox against every grant,
including ones nobody has created.

Two grants naming one environment variable cannot both be delivered, so the
narrower scope keeps it and the wider is passed over — silently swapping which
credential an agent's next command carries is the surprise this flow exists to
prevent. The variable itself lives on the grant (`SecretGrant.EnvName`) for the
same reason the binding is lazy: at project scope there is no binding yet to
carry the name.

Two paths mint that pair, and they mint the same thing:

- **Approving an agent's request**, which is the flow ADR 0031 describes: the
  agent says what it needs and why, and a person answers.
- **`CreateSecretGrant` with uses**, the pre-approval: somebody who already
  knows the answer grants it ahead of the asking. It carries the same
  obligations — sandbox scope, a concrete host, use IDs minted here, and an
  environment variable naming where the wrapped command receives it — because
  the thing being made is identical.

A grant with no uses is the ordinary standing kind: it authorizes the sentinel
the sandbox is already provisioned with, which anything in the sandbox can
read. An environment variable without uses is refused rather than ignored:
there is no delivery to name.

## The agent credentials broker

`agentcredentials.go` is the control-plane half of ADR 0031. A pool agent calls
it for one of its own sandboxes, and every entry point re-derives the sandbox
from the calling pool rather than trusting the caller about which sandbox it
speaks for. Nothing here returns a value.

The binding it creates on approval is a `SandboxSecret` marked `AgentRequested`,
and that flag is the whole of its difference: it never reaches the sandbox
environment, `secrets.json`, or the proxy's sentinel set. It exists so the pool
agent's ephemeral sentinels have a stable one to translate back to. The store
enforces this rather than each caller — `ListInjectedSandboxSecrets` is what
every injection path uses, and `ListSandboxSecrets` returns everything for the
few callers that need the full picture.

Only bearer and OAuth secrets work through this flow: `ResolveSandboxSecret`
emits `Value.Token` alone, so a git or ssh secret has nothing the proxy could
substitute. Extending the resolver is separate work.

## Two types, both doing work

A secret is a **token** or an **oauth** credential, and nothing else.

`token` is one opaque string. It is not called `bearer`: the proxy swaps the
value into whatever header the sandbox put it in — `x-api-key`,
`PRIVATE-TOKEN`, `Authorization` — so naming the type after one HTTP scheme
named a requirement nothing enforces. `oauth` is a token that rotates, and the
distinction is load-bearing: `ensureFreshOAuth` refreshes a near-expired access
token on resolve and the resolution's expiry is capped by the token's own
(ADR 0011). Both swap identically, because an OAuth secret's current access
token lives in the same field.

`git` and `ssh` are gone. Cleartext leaves through `ResolveSandboxSecret`,
which emits `Value.Token`, so a username/password pair or a private key never
reached a sandbox: no harness declared one, nothing installed a credential
helper, and a file-delivered one rendered a sentinel the proxy would not swap.
`migrateSecretTypes` renames stored `bearer` rows and deletes the git and ssh
ones with everything standing on them. It runs before `AutoMigrate` because the
API validates the enum on the way out as well as in, so one row left behind
would fail to serialize and take the whole secret listing with it.

## What the API says about a credential

The value goes in and never comes back. `SecretValue` is a request shape only:
the token, and for an OAuth credential the refresh token, token URL, client id,
scopes, subscription type and access-token expiry. That is enough to register
one by hand — a credential captured somewhere else, or rotated outside a
configure flow — rather than only through a harness's login.

What comes back is `Secret.oauth`: where it renews, whose client it is, what the
grant may do, when the access token goes stale, and whether it can renew itself
at all. Never the access token, never the refresh token. It is read out of the
encrypted value on the way past, because that is where the metadata was captured
with the tokens; a value that cannot be decrypted leaves the summary empty
rather than failing the read.

`oauth` means *renews itself*. Creating one without a refresh token and a token
URL is refused: what has been handed over is a token that will expire and stay
expired, and the type would promise a refresh nothing can perform.

## A secret's host is a binding somebody set

`Secret.Host` is optional and is only ever set by a person — `--host` at create
or update, or the approval dialog proposing the host a request named. Nothing
infers it: the provider table in the root `secretformat` package describes how
a credential is *shaped*, so a sentinel can byte-mimic it, and says nothing
about where it belongs. A host guessed from four leading characters is a
binding nobody chose, and being wrong in the narrow direction is how the
credential that plainly answers a request becomes the one an approver cannot
pick.

A secret that carries one may be used for that host and the hosts beneath it,
and nowhere else. That is checked twice, because the two checks answer
different questions:

- **`guardGrantHost`, when a grant is minted** — refuses an approval that would
  point the credential outside its binding, which is the typo worth catching
  while somebody is still looking at it.
- **`ResolveSandboxSecret`, when the value is handed out** — the same test
  against the destination the proxy observed, so a grant written before the
  binding existed does not outlive it.

A secret with no host is unconstrained by this, and the grant is what scopes it.

## A secret's grant limit is a ceiling, not a default

`Secret.MaxGrantTTL` is the longest a grant on that credential may live, and the
lifetime a grant takes when nobody names one. Both jobs, one number: the value a
person reads on the row is the value that binds.

It was only the second of those, and that made it decorative — every secret
carried the same 3600 nobody set, an approver could pass a longer lifetime or a
zero that never expires, and the row reported a policy the system did not have.

`guardGrantTTL` enforces it beside `guardGrantHost`, in `mintGrantAs`, for the
same reason: the lifetime arrives from an approval, a pre-approval, or the
in-sandbox flow, and a rule enforced in one of those is a rule the other two
walk around. Over the limit is refused, and so is a grant that never expires —
that case is named separately in the message, because it is not "longer"
arithmetically and it is the one an approver reaches for.

**Zero is the meaningful value "no limit"**, not an unset field: grants on such
a credential may live forever, which is how an unlimited credential says so out
loud. Two consequences follow. The column carries no GORM `default`, since GORM
omits a zero-valued field from an INSERT when one exists and would quietly
substitute an hour for "no limit"; the service owns the default a creator who
names none receives. And every writer says the lifetime explicitly rather than
dropping a zero, or the same substitution happens a layer higher.

Lowering a limit binds what is granted next, not what already stands: a live
grant is an authorization somebody made, and it is revoked deliberately rather
than shortened behind their back.

**A harness's own credential carries no limit.** The configure flow creates the
secret, binds it, and grants it at harness scope with no expiry — a harness that
stops working an hour after it was configured is a harness nobody configured —
so a ceiling there would describe a lifetime that grant does not have, and would
refuse the next grant somebody writes for it. That grant is minted through the
store rather than through `mintGrantAs`, which is why the limit never applied to
it either way; stating the zero is what makes the record agree with the
authorization. `liftConfiguredSecretGrantLimits` does the same for databases
written before this, and only for rows still carrying the old default.

## What a host scope covers

A host on a grant, a secret, or an approved use is a **scope**, read by the
root [`hostscope`](../../../../hostscope) package: it covers itself and
everything beneath it, and never its parent. `github.com` answers for
`api.github.com`; `api.github.com` answers for neither `github.com` nor
`uploads.github.com`.

The relation is one-way because the two directions are not the same act.
Narrowing a credential to a host under its binding is the approver being
careful. Widening one to the host above it hands the credential to a different
service — the one the site's other subdomains belong to — which is what the
binding said it was not for.

Three checks read it and must agree: `FindLiveGrant` matching the destination
the proxy observed, `guardGrantHost` refusing a grant outside a secret's
binding, and the pool agent's activation check. `FindLiveGrant` therefore
matches the host in Go rather than in SQL, and prefers the narrowest covering
grant (`hostscope.Specificity`).

## Sentinel shape

Sentinels are minted from the secret's `Format` through the root
`secretformat` package, so a placeholder is byte-shape-identical to a real
provider key. Both the stable binding here and the pool agent's ephemeral
sentinels come from that one function; a sentinel shaped by different rules at
each end would be distinguishable from the real thing.

## OAuth

An OAuth secret's access token rides in `SecretValue.Token` so the proxy swap is
identical to a bearer's. It is refreshed server-side on resolve when near
expiry, collapsed onto one upstream refresh by a singleflight group so a
rotating refresh token is spent once, and the resolution's expiry is capped by
the token's own so the proxy re-resolves as it ages out. See ADR 0011.
