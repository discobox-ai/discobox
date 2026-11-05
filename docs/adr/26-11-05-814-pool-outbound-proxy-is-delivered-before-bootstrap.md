# 26-11-05-814 — Pool outbound proxy is delivered before bootstrap

- **Status**: Accepted
- **Date**: 2026-11-05
- Accepted by the owner by merging #686, 2026-11-05.
- **Issue**: [#683](https://github.com/discobox-ai/discobox/issues/683)
- **Relates to**: 0096 (server configuration), 0020 §§5–7 and
  26-10-01-425 (nested container forwarding). Would narrow 0020 §4's
  unconditional injection and 26-10-01-425 §1's unconditional loopback rewrite
  for an explicitly managed pool exec/container; their ordinary sandbox and
  Docker bridge decisions remain in force. This also adds a VM transport.

## Context

A pool needs outbound access before its agent runs. The libkrun driver boots
its guest and Docker daemon before `dockerworker` creates the pool container
(`server/providers/libkrun/driver.go`, `server/providers/dockerworker/engine.go`).
Container environment alone cannot enable that daemon's first registry pull.
The pool bootstrap also fetches packages; #679 records its failure behind a
proxy. These reported failures have not been independently reproduced for this
proposal.

`proxy/upstream.go` already forwards HTTP and CONNECT through an upstream and
honors NO_PROXY, but selects one upstream for both schemes from environment.
`pool-agent/buildkitagent/buildkitagent.go` already forwards proxy environment
to buildkitd. Neither supplies configuration to the machines hosting them.
`server/internal/config/config.go` has no pool outbound setting. Its schema is
`server/config.schema.json`; the root `settings.schema.json` is repository
configuration and is not the place for this setting.

Inside a discobox, the inherited proxy names loopback. The Docker-facing
forwarder starts only with dockerd (`sandbox-agent/intake/activate_linux.go`).
ADR 0020 §7 deliberately lets Docker choose its subnet, and 26-10-01-425
rejects making the sandbox's own environment depend on docker0. A VM pool
must work without Docker running in its enclosing sandbox.

## Decision

### 1. Resolve outbound configuration for each pool

Add `pools.proxy` to `server.yaml`, generated through the configuration tags
and schema mechanism of 0096:

```yaml
pools:
  proxy:
    mode: explicit
    http: http://proxy.example:3128
    https: http://proxy.example:3128
    noProxy: .example.internal
    caFile: /etc/company/roots.pem
  overrides:
    pool_east:
      proxy:
        mode: explicit
        http: http://east-proxy.example:3128
        https: http://east-proxy.example:3128
        caFile: /etc/company/east-roots.pem
```

`mode` is `direct` (default), `explicit`, or explicitly selected `inherit`. Environment equivalents
are `DISCOBOX_POOLS_PROXY_MODE`, `DISCOBOX_POOLS_PROXY_HTTP`,
`DISCOBOX_POOLS_PROXY_HTTPS`, `DISCOBOX_POOLS_PROXY_NO_PROXY`, and
`DISCOBOX_POOLS_PROXY_CA_FILE`. Each environment value overrides its YAML
field, including an explicitly empty value.

- `inherit` resolves HTTP_PROXY and HTTPS_PROXY independently, uppercase before
  lowercase. A missing HTTPS value uses the HTTP value. NO_PROXY follows the
  same case precedence. ALL_PROXY is a last fallback only when it names an
  HTTP proxy. An explicitly present empty value suppresses fallback for that
  field. Setting the four explicit payload fields requires `mode: explicit`;
  otherwise startup rejects the ambiguity.
- `explicit` uses only the supplied fields; absent or empty HTTP/HTTPS means
  direct for that scheme. Both may be empty to configure extra trust alone.
- `direct` clears inherited proxy and bypass variables at every managed
  consumer. Payload fields with this mode are rejected.

The server's own proxy environment and a pool's outbound configuration are
separate. An ordinary installation with no pool configuration uses direct
pool egress even when the server has HTTP_PROXY set. Global defaults may
explicitly select `inherit`; merely changing the server's environment must
not change pools configured as direct or explicit.

Zero-configuration nesting is a distinct bootstrap action, not implicit
inheritance: when creating the initial pool inside a recognized Discobox,
read the enclosing sandbox's delivered proxy material and record an explicit
pool override with its trust material and a logical local-forwarder endpoint.
Resolve that endpoint through the backend at boot, never persist a Docker
bridge IP. Do this only when no pool-specific or global proxy configuration
was supplied; an explicitly configured global direct must win too. Existing
pools and later API-created pools are not silently rewritten. Show this
bootstrap-created override as such in status and allow normal API replacement
or removal. Removing it returns to YAML/global settings or direct, not another
automatic detection. Arbitrary server HTTP_PROXY values do not qualify as
recognized Discobox runtime material.

Initially support `http://` forward proxies, including CONNECT for HTTPS
origins, with optional URL userinfo for Basic authentication. Reject unsupported
schemes, paths other than `/`, query strings and fragments at configuration
load. HTTPS-to-proxy, SOCKS, PAC and interactive enterprise authentication are
out of scope; revisit when a deployment needs them. Redact userinfo in all
status, diagnostics and errors; deliver credentials only in private runtime
material, never API responses or command arguments.

The global `pools.proxy` is the default, not a restriction to one proxy for
all pools. `pools.overrides` is a map keyed by immutable pool ID, with a
`proxy` object using the same configuration shape. It may name a pool before
creation; an unmatched entry does not create a pool. Names and provider IDs
are not selectors, so renaming a pool or sharing a provider cannot retarget an
override.

Add an optional `proxy` object to pool create and update in the public API,
and expose its redacted configured value and effective source on pool reads.
The API object has the same modes, endpoints and bypass semantics, with
`caPem` containing additional PEM anchors instead of a server-local `caFile`.
The API must not allow callers to read arbitrary server files. Persist this
object as pool intent through an additive database migration; existing rows
have no override. Use the existing pool administration authorization for these
writes. Proxy credentials are encrypted at rest through the server's existing
secret encryption facility, never returned on reads; omission of credentials
on a redacted read is not an instruction to clear them. The write contract must
provide explicit credential replacement/removal without requiring readback.

Precedence is a persisted API pool override, then the pool-ID entry in
`server.yaml`, then global `pools.proxy`, then its default `direct` behavior.
Select a whole object, without merging individual endpoint or trust fields
across levels. Thus a pool's `direct` can disable a global proxy, and its
`explicit` empty HTTPS never inherits the global HTTPS endpoint. Pool-level
`inherit` means use the server's standard proxy environment directly; removing
the override means use the next configuration level. On create, omitted or
null `proxy` leaves no API override. On update, omission preserves it and null
removes it, revealing the YAML/global default. YAML reload never overwrites
persisted API intent; status shows when an API override shadows a YAML entry.
Global `DISCOBOX_POOLS_PROXY_*` variables change the global default only, not
individual pool overrides. Per-pool configuration is available through YAML
and the API without requiring per-pool environment variable names.

Resolution happens once per pool in the server, not independently in each process.
Extend the required provider/boot contracts with a domain-owned value carrying
both schemes, bypasses and CA bytes. Consumers must distinguish resolved direct
from unresolved: empty fields must not reactivate environment fallback in
`proxy.Config`. Preserve the proxy's reusable environment-based entry point
for standalone callers, while managed pools always receive resolved settings.

### 2. Deliver trust and routing before the first fetch

`caFile` in global or per-pool YAML is a server-local PEM file, read and validated by the server, then
copied as bytes. Never hand its host path to a guest. In inherited mode,
collect the server's effective PEM trust inputs (`SSL_CERT_FILE` and
`SSL_CERT_DIR` where set), including the enclosing discobox's delivered CA.
Do not assume another OS has the same root paths. Explicit mode adds only
`caFile`; system roots in each destination remain present. Unreadable configured
trust fails validation; an absent additional bundle leaves system trust alone.

Install extra anchors by fingerprint and regenerate destination stores without
replacing stock roots. Keep outer, enterprise and inner pool CAs distinct.
Never disable TLS verification. Never bake deployment trust or proxy credentials
into an image or bundle.

Required delivery boundaries are:

| Boundary | Required behavior |
| --- | --- |
| Server artifact downloads | Retain the server's own outbound configuration; a pools-only setting does not reroute the server. Diagnose this hop separately. |
| External Docker daemon | Verify its registry access before pool creation; a remote or operator-owned daemon cannot be configured by container environment. Report how to configure its proxy/trust rather than claiming success. |
| Managed VM guest | Deliver configuration over its existing host/guest boot channel, before starting dockerd or any network bootstrap. Include libkrun, vz and other managed VM drivers. |
| Hosted pool machine | Include configuration/trust in initial machine delivery before package installation or image pulls; a server-loopback proxy is invalid unless a driver supplies a relay. |
| Pool container and provisioning exec | Place anchors before any fetch and supply resolved environment explicitly to container creation, every Docker ExecConfig used for provisioning, and systemd services. The initial process environment does not configure a later `docker exec`. This includes apt refresh in #679; never retry direct on proxy failure. |
| Pool proxy | Use separate HTTP/HTTPS upstreams and one NO_PROXY matcher for inspected requests and passthrough CONNECT, including pinned transports. |
| Pool Docker and BuildKit | Configure daemon pulls, buildkitd registry requests and build execution through their existing environment/trust delivery. Build steps keep sandbox identity and the pool proxy. |

For managed pool containers and provisioning execs, the resolved configuration
is authoritative over enclosing `runcca` injection. Pass a reserved
`DISCOBOX_PROXY_ENVIRONMENT=managed` marker in the container and every
provisioning ExecConfig environment. The wrapper recognizes it in the OCI
process environment before injection or loopback retargeting: retain the
caller-supplied resolved proxy environment and do not inject or rewrite proxy
or proxy-trust variables. Strip the marker before executing the target process.
Supply it independently on exec, since removing it from the initial OCI spec
must not determine later exec behavior. Docker labels are insufficient: the
marker must reach the actual create and exec process specs. If a daemon cannot
carry it, fail the managed operation with a capability diagnostic rather than
claiming direct mode was applied. Exercise both paths on the supported daemon.

In direct mode, supply no ambient HTTP_PROXY, HTTPS_PROXY, ALL_PROXY or
NO_PROXY variables in either case, including lowercase and tool-specific proxy
aliases that the outer manifest would inject. Clear inherited proxy-trust env
references too; use only the destination's system roots and separately managed
trust. In explicit mode, preserve exactly the resolved per-scheme settings,
including an empty/direct scheme. Suppressing injection does not suppress the
enclosing sandbox's firewall or confer direct network access: direct mode in
a proxy-only sandbox may correctly fail. The marker is a routing ownership
signal, not an authorization boundary; sandbox users already control nested
container environment. Ordinary unmarked containers retain existing injection
and retargeting behavior.

The base-image pull belongs to the hosting daemon, before container creation.
A Docker pool cannot repair that daemon from inside its own container. Managed
VM daemons are configured by Discobox; operator-owned daemons are a documented
prerequisite with a diagnostic. This distinction applies to fresh installation.

### 3. Nested VMs use a private host relay, independent of dockerd

Keep sandbox processes on their loopback forwarder and nested containers on
the existing dynamic Docker bridge. Do not start dockerd, pin a subnet, bind
an unauthenticated forwarder to all interfaces, or publish the outer pool's
client certificate into a nested guest to enable VM egress.

For a local VM driver, add a per-pool byte relay over its host/guest channel
(VSOCK for libkrun and vz). The host end dials the resolved upstream as the
server process can reach it, including sandbox loopback. A guest-local HTTP
listener forwards to that relay and is the guest's proxy URL. The relay does
not terminate HTTP or substitute identities; the enclosing sandbox's forwarder
continues to authenticate and enforce that sandbox's egress policy. Separate
HTTP/HTTPS endpoints may share a relay only when they resolve to the same hop.
Scope host sockets to the server user and pool, and stop them with the VM.

This removes any dependence on passt's mapping of host loopback. The guest-local
listener also needs a container-reachable endpoint for the pool container:
use its existing dynamic Docker bridge forwarding mechanism after dockerd
starts, with readiness before starting that container. The VM's daemon itself
uses the guest loopback endpoint, which is available before dockerd starts.

In inherited mode this translation is automatic for a recognized enclosing
Discobox loopback forwarder. Explicit configuration remains the operator's
choice; a local VM driver may relay a host-reachable explicit endpoint too.
A hosted machine cannot use a host-local relay without a supported transport;
reject an unreachable local endpoint before provisioning, rather than silently
substituting direct egress. An enclosing denial remains a denial: nesting
confers no additional network permission.

### 4. Exempt internal routes without weakening egress policy

Keep `pool-agent/internalhttp` transports explicitly proxy-free for the control
plane and sandbox agents. Preserve Unix sockets, VSOCK and runtime dialers;
NO_PROXY is not a replacement for these routes.

For ordinary environment-aware clients, merge operator NO_PROXY with exact
internal control endpoint hosts, loopback and runtime-discovered directly
connected addresses needed for local pool services. Do not add all RFC1918
space, public registry domains, or a wildcard. Retain standard suffix, CIDR,
port and `*` matching for explicit operator entries. Bypassing the upstream
never bypasses the pool proxy's destination restrictions, allowlist, identity,
secret policy, or loopback protection. Pool-owned asset/origin/registry hosts
remain handled locally by the pool proxy.

### 5. Changes converge without replacing durable state

Server restart reloads and validates YAML configuration and CA bytes. API
updates validate and persist pool intent in the same transaction as the pool's
reconcile dirty mark. Recompute only pools whose effective configuration
changed, including when an override is removed or a global default changes.
Publish a
content revision with each pool's desired runtime configuration and reconcile
it through the existing engine. Running pools report desired and applied
revisions. Apply environment/trust atomically, restart only affected consumers,
and retain a failed apply as an actionable error for retry. Do not automatically
recreate pools, erase databases, or remove sandbox/data/cache volumes.

Where a guest boot transport cannot apply the change live, report restart
required and apply it on the next ordinary pool restart. Do not force-stop
running workloads to rotate configuration. Explicitly retiring previously
managed anchors must remove those anchors without removing unrelated trust.
Existing pools keep their last applied revision until the replacement is valid;
new pools must not bootstrap with stale or partially delivered material.

### 6. Diagnose each network hop

Expose each pool's effective mode/source (API override, YAML pool override, global default or environment), redacted endpoints, CA fingerprints, revision and
restart-required state through server status/doctor. Run bounded probes from
the actual server, guest and pool contexts where available: upstream connection,
CONNECT/HTTP response, TLS verification and registry access. Distinguish proxy
authentication/denial, unreachable proxy, untrusted CA and direct-path failure.
Before a guest exists report its path as untested, never healthy merely because
the server's probe passed. A direct probe failure with no configured proxy
suggests configuring one; it is evidence of that attempted path failing, not
proof that every outbound route is impossible. No silent direct fallback.

## Alternatives rejected

- **Implicitly inherit the server proxy for every pool.** The server and a
  target pool can use different network paths. Default direct preserves that
  separation; explicit inherit and recognized nested bootstrap cover deliberate
  reuse without turning every ambient server variable into pool intent.
- **Only pass server environment to the pool container.** Misses guest daemon
  pulls, trust placement and host loopback translation.
- **Start the nested Docker bridge for all VM pools.** Adds dockerd as a VM
  dependency and does not solve host-to-guest address translation independently.
- **A universal fixed-address bridge.** Reintroduces subnet/address collisions
  at each nesting level, the reason 0020 §7 removed the pinned Docker subnet.
- **Global-only configuration or provider-level overrides.** A pool may need
  a different network path from another pool using the same provider; neither
  gives the owner the requested pool-specific API and YAML controls.
- **Merge individual fields across levels.** Makes an omitted endpoint or CA
  unexpectedly inherit a different pool default and weakens explicit direct
  semantics. Whole-object selection gives an override one complete meaning.
- **Configure each component independently.** Makes explicit direct mode and
  trust rotation inconsistent and lets environment fallback undo operator intent.
- **Replace existing pools when settings change.** Risks durable state and
  interrupts workloads for a configuration update.

## Consequences and validation

Implementation spans configuration, provider contracts, guest boot, pool
runtime, proxy upstream selection and diagnostics. This PR changes no runtime
behavior and leaves this record Proposed. Update current design docs with the
implementation, not with this proposal.

Acceptance requires proxy-only fresh Docker and libkrun installs, a sandbox
and ghcr.io pull in each, BuildKit pulls and RUN requests, apt refresh without
direct retry (#679), and a zero-configuration nested libkrun run using the bootstrap-created explicit override with enclosing
dockerd stopped. Cover two pools using different proxies on one provider, independent API and
YAML pool overrides, API precedence and null removal, unchanged unrelated pools,
redacted credential updates and durable migration. Also cover distinct scheme
endpoints, default direct despite a server proxy, explicitly selected inherit,
explicit direct/empty values through both marked container create and provisioning
exec under outer runcca, unchanged unmarked injection,
a private enterprise CA, inherited outer and inner CAs together, internal
control traffic, upstream refusal, and an existing pool configuration update
preserving its sandbox/data volumes. Exercise vz and hosted delivery as well;
mark unavailable provider coverage explicitly rather than inferring it from
libkrun.

Exact boot-message encoding, relay port allocation and diagnostic probe targets
remain implementation choices. Supporting a hosted provider's missing private
relay is deferred until it has such a transport; it must reject host-local
proxy configurations meanwhile. HTTP proxy transport beyond `http://` is
revisited on a concrete enterprise requirement.
