# 26-11-05-814 — Pool outbound proxy is delivered before bootstrap

- **Status**: Proposed
- **Date**: 2026-11-05
- **Issue**: [#683](https://github.com/discobox-ai/discobox/issues/683)
- **Relates to**: 0096 (server configuration), 0020 §§5–7 and
  26-10-01-425 (nested container forwarding). Their loopback and Docker
  bridge decisions remain in force; this adds a separate VM transport.

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

### 1. Resolve one server-owned outbound configuration

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
```

`mode` is `inherit` (default), `explicit`, or `direct`. Environment equivalents
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

Initially support `http://` forward proxies, including CONNECT for HTTPS
origins, with optional URL userinfo for Basic authentication. Reject unsupported
schemes, paths other than `/`, query strings and fragments at configuration
load. HTTPS-to-proxy, SOCKS, PAC and interactive enterprise authentication are
out of scope; revisit when a deployment needs them. Redact userinfo in all
status, diagnostics and errors; deliver credentials only in private runtime
material, never API responses or command arguments.

Resolution happens once in the server, not independently in each process.
Extend the required provider/boot contracts with a domain-owned value carrying
both schemes, bypasses and CA bytes. Consumers must distinguish resolved direct
from unresolved: empty fields must not reactivate environment fallback in
`proxy.Config`. Preserve the proxy's reusable environment-based entry point
for standalone callers, while managed pools always receive resolved settings.

### 2. Deliver trust and routing before the first fetch

`caFile` is a server-local PEM file, read and validated by the server, then
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
| Pool container | Place environment and anchors before starting its agent/bootstrap, covering apt refresh in #679. Do not retry direct when the proxy fails. |
| Pool proxy | Use separate HTTP/HTTPS upstreams and one NO_PROXY matcher for inspected requests and passthrough CONNECT, including pinned transports. |
| Pool Docker and BuildKit | Configure daemon pulls, buildkitd registry requests and build execution through their existing environment/trust delivery. Build steps keep sandbox identity and the pool proxy. |

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

Server restart reloads and validates configuration and CA bytes. Publish a
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

Expose effective mode/source, redacted endpoints, CA fingerprints, revision and
restart-required state through server status/doctor. Run bounded probes from
the actual server, guest and pool contexts where available: upstream connection,
CONNECT/HTTP response, TLS verification and registry access. Distinguish proxy
authentication/denial, unreachable proxy, untrusted CA and direct-path failure.
Before a guest exists report its path as untested, never healthy merely because
the server's probe passed. A direct probe failure with no configured proxy
suggests configuring one; it is evidence of that attempted path failing, not
proof that every outbound route is impossible. No silent direct fallback.

## Alternatives rejected

- **Only pass server environment to the pool container.** Misses guest daemon
  pulls, trust placement and host loopback translation.
- **Start the nested Docker bridge for all VM pools.** Adds dockerd as a VM
  dependency and does not solve host-to-guest address translation independently.
- **A universal fixed-address bridge.** Reintroduces subnet/address collisions
  at each nesting level, the reason 0020 §7 removed the pinned Docker subnet.
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
direct retry (#679), and a zero-configuration nested libkrun run with enclosing
dockerd stopped. Cover distinct scheme endpoints, explicit direct/empty values,
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
