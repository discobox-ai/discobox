# Pool-Agent Request Tokens Design

Control-plane calls to a pool agent, and to the sandbox agents behind it, use a
separate server-owned issuer key managed by `poolagentauth.Manager`. The public
key is delivered to the pool host in bootstrap metadata as
`DISCOBOX_CONTROL_PLANE_PUBLIC_KEY`, and the pool agent passes it on to the
sandbox agents it runs. The private key remains on the control plane in
`server_state` under `worker_agent_request_issuer`. It is sealed at rest, in the
keyring's `server` scope, when an encryption provider is configured and stored
in plaintext otherwise; `Reseal` brings an older one up to date at startup.

`server/providers/poolruntime` mints short-lived PASETO v4.public bearer tokens
through an auth token provider on each pool-agent client lease. Tokens are:

- audience-bound to `pool-agent` (verified by `pool-agent/server`) or
  `sandbox-agent` (verified by `sandbox-agent/server`; exec, terminal, and TCP
  scopes need one);
- carry `project_id`, `pool_id`, optional `sandbox_id`, and operation `scopes`;
- valid for 15 minutes, with `iat`/`nbf` backdated 5 minutes for local VM
  clock skew.

The pool agent rejects tokens whose project and pool do not match its own
identity and the route. A pool principal can also obtain `status:read`-only
sandbox-agent tokens for sandboxes it hosts, through
`MintSandboxAgentStatusTokens`.

Driver HTTP leases (`transport.HTTPClientLease`) should carry
routing/connectivity and a token provider only; they should not cache or
persist pool-agent request tokens.
