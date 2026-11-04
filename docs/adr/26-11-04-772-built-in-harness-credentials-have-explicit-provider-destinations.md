# 26-11-04-772 — Built-in harness credentials have explicit provider destinations

- **Status**: Proposed (would extend [26-10-02-924](26-10-02-924-a-secret-may-carry-routes-and-every-request-it-is-sent-in-must-fall-on-one.md) §2 to save-back providers and narrow its replacement of harness-owned routes; would narrow [26-10-27-939](26-10-27-939-a-route-may-name-any-host.md) §2's host/anywhere fallback for built-in harness-collected credentials)
- **Date**: 2026-11-04
- **Issue**: [#576](https://github.com/discobox-ai/discobox/issues/576)

## Context

The [audit](https://github.com/discobox-ai/discobox/issues/576#issuecomment-6623725552)
at `5f55b87ed782daa82ac5199c8288f906e0876b7e` found that checking only
`harness.secrets` misses most built-in credentials. OpenCode, omp and pi each
register 27 `harness.saveBack.providers`; none declares routes. Their credentials
are applicable even though their static secret lists are empty.

After [#555](https://github.com/discobox-ai/discobox/pull/555) merged,
[26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md)
removed configure. On main at `9aedd971`,
`server/internal/resources/harnessconfigs/secrets.go:configuredRoutes` retains
the exact-host/`*` fallback. `capture.go:applyCapturedCredential` uses registered
provider routes, preserves an owned secret's policy when those are absent, and
uses the provider host or `*` for a new secret. Claude's providers supply
`api.anthropic.com`, Codex's API-key provider supplies `api.openai.com`, and
Codex's OAuth provider supplies none. Copilot declares routes in both static
and provider metadata. Pi's Copilot provider supplies `api.github.com`.
`secrets.go:syncConfiguredSecretRoutes` still synchronizes static declarations
onto owned secrets at bundle refresh. `ConfiguredSecretIDs` keeps its name and
includes credentials created before configure was removed.

People now sign in inside a working box and explicitly save a captured change.
Before save and sentinel handoff, the vendor credential file can contain a raw
login readable by that box's processes (152's Consequences). This proposal's
managed-secret routes protect spending after capture and delivery; they do not
promise to constrain an unregistered local login before handoff. General egress
policy remains a separate boundary.

`pool-agent/proxyagent/secrets.go:Authorize` already enforces every sentinel's
held route lists. An off-route request is refused before credential resolution.
The audit ran existing proxy integration tests and pool route-decision tests;
they passed, including refusal without upstream delivery. They do not establish
all destinations used by any vendor CLI. No live vendor traces were collected.

A provider name does not identify a single destination: Azure, regional cloud
services, enterprise Copilot and custom base URLs are concrete counterexamples.
Choosing a wildcard or trusting a configured URL decides where real credentials
may leave the pool. That is a trust policy, rather than missing JSON alone.

## Decision proposed

### 1. Every supported credential has provider-owned destination policy

Each built-in static secret and each supported save-back provider/login type
must declare explicit routes for the credential the sandbox actually sends.
Capture and bundle refresh consume the same authoritative policy.
Static declarations and provider declarations for the same credential must agree.
Multiple accounts of one provider use that same policy unless a person supplies
a distinct endpoint-specific secret.

A hostless built-in credential must not acquire `*` implicitly. Do not treat
`rejectionScope`, `egressHosts`, OAuth client metadata or the absence of static
secrets as authorization. Shell has no harness-collected credentials and is
explicitly not applicable. Unsupported local login forms stay unsupported;
listing a provider does not make every vendor login form supported.

The built-in inventory check covers both `secrets` and `saveBack.providers`,
including each accepted credential type. A non-applicability reason must explain
why no credential is collected or delivered; it cannot excuse an unrestricted
credential that is in fact delivered. An intentionally broad destination policy
requires explicit routes and a reviewed rationale, never an omitted policy.

### 2. Custom destinations require a person's explicit policy

Built-in defaults authorize reviewed vendor destinations only. A portable
preference containing a base URL, an account's metadata, or a capture
command's output cannot expand credential destinations. A person using a custom,
enterprise or regional endpoint supplies a secret with explicit endpoint routes
and binds it to the harness config through the existing secret/binding API.
The harness does not overwrite that manually bound secret's policy.

A login whose destinations cannot be represented safely is refused with a
value-free explanation and instructions to supply explicit endpoint policy.
Do not silently preserve functionality by granting `*` or a broad cloud suffix.
This deliberately trades automatic portability of captured logins for an
explicit decision by the person who owns the credential.

### 3. Access-token spending and server-side refresh are separate

A sandbox's access-token routes describe where that access token may be sent.
The server's registered OAuth refresh recipe describes where the server sends
the refresh token. `server/internal/resources/secrets/oauth.go` refreshes outside
the sandbox proxy's route check. A `tokenUrl` alone is therefore not grounds for
adding its host to the sandbox access-token routes.

A harness that itself sends a delivered credential to a token-exchange endpoint
needs that endpoint in its spending routes. Pi's GitHub-to-Copilot exchange is
one such flow: distinguish the GitHub credential from the derived token. Do not
expand the source credential's routes to every destination the derived token
uses. This proposal does not change server refresh authorization or deliver
refresh material that the current model keeps server-side.

### 4. Destination evidence gates the concrete route table

The issue audit establishes missing policy, not a verified vendor route table.
Before implementation, establish the destination host patterns and any useful
method/path restrictions for every supported provider and login form, against
the shipped CLI version, from primary vendor source/documentation and observed
traffic where needed. Record provenance with the owning harness policy.

Start that evidence with these distinct flows; these are investigation targets,
not an approved exhaustive allowlist:

| Harness | Flows requiring evidence |
| --- | --- |
| Claude Code | API key and OAuth access-token spending at Anthropic; any additional authenticated first-party calls; server refresh separately |
| Codex CLI | OpenAI API-key calls versus ChatGPT subscription calls; server refresh at the registered token endpoint separately |
| Copilot | Existing Copilot model paths, selected GitHub API paths and explicit blocks; compare policy in both declarations |
| OpenCode, omp, pi | Every accepted token/OAuth provider, per-type endpoints, regional/custom services and exchange flows; all 27 registered provider entries per harness must be accounted for |
| shell | No harness-collected credential; independently bound secrets keep their own policy |

Do not add method/path restrictions merely by guessing the inference endpoint:
quota, account, model-listing and auxiliary authenticated calls may also be
required. An unverified destination is an explicit open item that blocks
completion of #576, rather than an assumed default. Revisit a policy whenever
the shipped harness changes its authenticated traffic.

### 5. Existing credentials converge without widening restrictions

Persisted databases and credential values remain intact. Reconcile existing
harness-owned credentials against the new provider policy, including legacy configure-created secrets and those
created through capture, without requiring another login. Apply new restrictions
as an additional intersection with existing effective restrictions, preserving
blocked routes and method/path limits; do not approximate an intersection by
unioning allowlists. Bindings remain an independent restriction under
[26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
§§1, 4. This narrows 924 §2's replacement rule for harness-owned secrets. It does not restore
configure or alter 152's sign-in and explicit-save lifecycle.

Use an atomic, idempotent durable update and mark holders for reconciliation in
the same transaction, as the current static route sync does in `secrets.go`. Report credentials
whose prior custom destination no longer works and require explicit person-owned
endpoint policy; do not silently broaden them. Manually bound secrets are never
rewritten. The exact persisted representation for intersecting old and new
policy is open to implementation; it must prove that an old denial cannot turn
into an allowance, including overlapping patterns and blocked routes.

### 6. Verification must prove policy and plumbing together

A built-in inventory validator fails for a delivered credential with neither
routes nor a valid non-applicability declaration. It also catches static/provider
policy divergence. Exercise first capture/save, existing-credential refresh,
manual bindings and the durable upgrade; a JSON-only check is insufficient.

Keep the existing route and proxy tests. Add a test that takes the actual Claude
policy through publication and the real pool resolver into the proxy, verifies
an allowed request, then verifies an off-route request receives 403 without
credential resolution or upstream delivery and produces a blocked audit entry.
The existing Claude-shaped-sentinel test uses a stub denial and is not that test.
Run the accepted login flows to establish that permitted destinations still work.

## Alternatives rejected

- **Only add routes to Claude and Codex static secrets.** This leaves the 81
  multiprovider entries and capture policy source uncovered.
- **Trust the capture-reported host or settings base URL.** That lets sandbox
  output or portable preferences decide where a real credential may be sent.
- **One shared wildcard policy per vendor.** Regional and enterprise endpoints
  do not justify authorizing unrelated tenants or services under a broad suffix.
- **Add OAuth token hosts to every access-token allowlist.** Server refresh and
  sandbox access-token spending use different credentials and enforcement paths.
- **Replace all existing routes with defaults.** That can undo a person's
  narrower restrictions and breaks endpoint-specific credentials without an
  explicit transition.

## Consequences and open items

Acceptance authorizes the policy above, not an unmeasured list of vendor hosts.
The concrete provider/type route table and the durable intersection representation
remain open. Implementation cannot claim #576 complete until both are resolved
and verified. No runtime, manifest, database, or current-state design document
changes with this proposal. The general `*` route and inline-secret behavior of
939 remain available; this proposal narrows only built-in harness collection.
