# 0106 — A trusted judge checks requests before credential substitution

- **Status**: Proposed
- **Date**: 2026-09-10
- **Amends**: [0079](0079-a-local-judge-gates-every-wrapped-credential-use.md) §5's deferred trusted-side placement and its rejection of swap-time judging; settles [0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md) §6's deferred request judge.

## Context

`discobox-access run` judges a command against an approved use before asking
for an ephemeral sentinel. The pool records that verdict and binds the sentinel
to a sandbox, use ID, host, and expiry. The proxy checks those bindings when
resolving the sentinel, then caches the credential by sandbox, sentinel, and
host. Nothing compares the actual HTTP operation with the approved use.

The sandbox can declare one command and execute another. Moving the command
judge to `get` would authenticate who made the verdict, but would still judge
the sandbox's declaration. The trusted proxy can observe the actual request.
That supports a different question from ADR 0079's command check: whether this
request is reasonably associated with the approved purpose.

## Decision

### Judge each use-scoped request before substitution

The proxy detects all matching sentinels in the original request using the
existing exact-set matcher, including base64 handling. Pool-agent binds each
ephemeral sentinel to its live activation under the authenticated sandbox
identity. The control plane resolves the activation's use ID to its current
approved description and checks that the use, credential, sandbox, and host
belong to a live grant. Neither the authoritative description nor the use ID
comes from a sandbox-supplied HTTP header.

An LLM judges the observed request against that description. All applicable
uses must pass before any credentials are substituted. Missing configuration,
timeout, malformed output, refusal, expired activation, revoked grant, or
failure to persist the verdict leaves all sentinels in place. Preserve the
proxy's existing fail-closed-on-the-secret behavior: the request can proceed
upstream carrying placeholders, without a real credential.

Authorization is a required operation of the resolver contract, separate from
credential resolution. It runs before consulting the credential cache and
guards the previous-value retry path too. A credential cache hit never
authorizes a new operation. Start without a verdict cache. A byte-identical
retry within the same in-flight request may reuse its recorded verdict, but
must recheck activation and grant liveness. Changed request data needs another
verdict. Background credential refresh cannot manufacture an authorization.

Static harness credentials without a use-scoped activation retain their
existing host/grant policy: they have no approved-use sentence. Credentials
issued through the access protocol remain activation-only; exposing their
underlying stable sentinel would bypass the association with the use ID.

### Ask about reasonable association, including supporting operations

The question is:

> Is this HTTP request reasonably part of carrying out the approved use,
> including ordinary supporting operations, without materially expanding it?

For “open a PR in org/repo,” looking up that repository, reading its branches,
and creating the PR can all qualify. Deleting the repository or changing
organization membership does not. Supporting reads still need a relevant
target and purpose; being read-only is not sufficient. GraphQL queries and
mutations must be distinguished by their body, not their shared POST endpoint.

The declared command is optional, explicitly untrusted context. It cannot
override the observed request. Request content is data to evaluate, never
instructions to the judge. Text asserting approval cannot grant itself scope.
The model has no tools and returns a strict allow/reason verdict; only an
explicit allow passes. This is semantic screening, not proof that uploaded
content is correct or that an operation achieves the user's intent.

### Execute the model on the control plane

Each project designates a judge model and a model-provider secret held by the
control plane. This selects whose account pays and which provider receives
request context. The control plane calls that provider directly, without
tools or sandbox-controlled harness configuration. Its call does not travel
through the sandbox credential substitution path, avoiding recursive judging.

Pool-agent sends request evidence and the activation binding over its existing
authenticated control-plane connection. It holds no new model credential. The
control plane loads the approved description itself. The model never receives
the credential being released.

Missing configuration disables use-scoped substitution. There is no fallback
to the sandbox's model or its reported verdict. The local command judge remains
useful for feedback before execution but does not satisfy the request gate.

### Bound the evidence and preserve the outbound bytes

Evidence includes method, destination authority and port, path, query, relevant
headers, and the body when present. Capture it before substitution or
secret-bearing header injection. Redact authentication material and sentinel
values before sending evidence to the model or recording it.

Inspection has explicit byte and time limits, including decompression limits.
Buffering and decoding for inspection must not alter the outbound bytes. An
oversized, unreadable, unsupported, or incompletely captured body fails closed:
showing only a prefix cannot authorize an unseen suffix. Bodyless requests
remain judgeable. Large uploads and streamed Git operations initially require
a bounded protocol parser whose summary preserves the operation and target
being authorized before they can use this path.

Judging a WebSocket handshake does not authorize later messages. Use-scoped
credentials are not substituted into protocol upgrades until that protocol has
an enforcement path for subsequent operations.

### Persist a trusted verdict distinct from the command verdict

Persist the verdict before releasing the credential. Record its origin as
trusted request enforcement, sandbox, use and grant IDs, proxy request
correlation ID, model and prompt version, redacted evidence, allow/reason, and
latency. A sandbox-reported verdict cannot claim that origin. Associate denials
and judge failures with the proxy audit record even when no substitution
occurs. Credential values must stay out of prompts, verdicts, spools, and cache
keys.

## Alternatives rejected

**Judge only when minting the sentinel.** It judges a claimed command rather
than the operation receiving the credential, even if its model runs on the
trusted side.

**Judge inside the existing cached resolve operation.** An allowed read would
authorize a subsequent delete at the same host until the credential cache
expired. Credential freshness and request authorization have different keys
and lifetimes.

**Run a harness on the pool host.** It can be trusted, but adds a harness,
model authentication, and configuration lifecycle to every pool. The control
plane already owns project configuration, secrets, grants, and verdict
persistence. Central execution keeps that ownership together.

**Send only method and URL to keep judging cheap.** GraphQL and other APIs put
the operation, target, or scope in the body. Those fields can be decisive.

## Consequences

- Use-scoped requests gain a model call and durable verdict write. Bound
  concurrency and deadlines to contain load and latency.
- Projects must configure the judge before access-protocol credentials work
  under this policy. Existing databases and grants survive; configuration and
  verdict changes use additive migrations.
- Proxy and pool/control-plane contracts change. The portable agent credentials
  protocol and sentinel format do not need to change.
- False refusals and model outages can stop legitimate use. Model mistakes can
  still allow misuse; deterministic identity, host, grant, and expiry checks
  remain mandatory.
- Large or opaque bodies need protocol-aware inspection. Choosing an encoding
  the judge cannot understand cannot exempt a request from the check.
