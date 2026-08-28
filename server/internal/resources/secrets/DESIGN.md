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
