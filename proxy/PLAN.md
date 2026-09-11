# Proxy Implementation Backlog

> **Status: fully implemented.** Every item below has shipped, including the
> pool integration and all three Sentinel Secret Swapping phases. This file is
> kept as the record of the backlog; the current design lives in
> [`DESIGN.md`](DESIGN.md), and the pool-side wiring in
> [`pool-agent/DESIGN.md`](../pool-agent/DESIGN.md).

## Pool Integration (done)

- The pool proxy runs as the `discobox-proxy.service` systemd unit inside the
  pool container (`discobox-pool-agent proxy`); `pool-agent/proxyagent`
  prepares the CA bundle (`PrepareBundle`) before systemd boots.
- The sandbox-local forwarder lives in the dependency-light `proxy/bridge`
  package and runs as `discobox-proxy-bridge.service`
  (`discobox-sandbox-agent proxy-bridge`).
- `sandboxruntime.CreateSandbox` issues per-sandbox client certificates (client
  ID = sandbox ID), places the public CAs + client keypair in the config volume
  so they appear in the sandbox at `/etc/discobox/proxy`, injects proxy/CA env
  into the container and manifest, and attaches the sandbox only to the
  per-pool internal network (`discobox-sbnet-<poolID>`), where the pool is
  aliased `discobox-pool-proxy`.
- The pool proxy is built with a real `proxy.SecretResolver`
  (`proxyagent.secretResolver`), so sentinel secret swapping is live.

## Work Items

- [x] Add OpenTelemetry spans on the core datapath: mTLS accept, protocol
  detection, HTTP request handling, CONNECT/MITM, header rewrite, cache
  lookup/store, audit enqueue/drop/write, and SOCKS connect handling.
- [x] Add sandbox-local forwarding proxy mode that accepts localhost proxy traffic
  and forwards to the pool proxy with the sandbox client certificate.
- [x] Add end-to-end mTLS HTTP proxy tests covering client identity, blocked
  requests, CONNECT/MITM, cache hit/store, and header injection.
- [x] Add SOCKS integration tests covering mTLS client identity and allow/deny audit
  records.
- [x] Expand audit schema and tests for timing details, cache event details,
  blocked CONNECT metadata, dropped audit counters, and disk-spooled body/stream
  metadata.
- [x] Port or redesign upgraded-stream/WebSocket recording.
- [x] Add disk-spooled request/response body capture with control API retrieval.
- [x] Add file-watched runtime config updates; do not expose an HTTP runtime
  config API from the proxy.
- [x] Add certificate rotation and expiry handling beyond create-or-reuse.
- [x] Avoid or isolate `goproxy` process-global CA/action mutation if multiple proxy
  instances in one process are required.
- [x] Add cache eviction, startup index loading, and corruption handling tests.
- [x] Add a read-only control API for client-scoped audit queries and upgraded
  stream spool retrieval.
- [x] Expand header rewrite matching with path, method, and client identity
  conditions for secret injection.
- [x] Add explicit config, rule, and certificate option validation.

## Sentinel Secret Swapping

Give sandboxes fake sentinel credentials and swap them for real values at the
proxy, authorized per destination host. See `DESIGN.md` → Sentinel Secret
Swapping.

- [x] Phase 1 — proxy core: `internal/secrets` sentinel matcher (headers +
  query), `Resolver` interface, TTL positive/negative cache; `Config.Secrets`
  with `ApplyConfig` hot-swap; swap stage in the HTTP request path; audit
  redaction of swapped headers and pre-swap URL for query swaps; unit and
  end-to-end tests (swap reaches upstream, real value never audited, host-denied
  sentinel left in place).
- [x] Phase 2 — server: `Secret.Format` field + `secretformat` generative-template
  package (generate sentinel, validate inline value, reverse-infer format without
  leaking entropy); seed provider table (format + default host); anonymous-secret
  creation for inline values (`Anonymous`/`UniqueKey` columns exclude them from
  the type+host uniqueness domain and from list/match); `SandboxSecret` assignment
  table + store; `SandboxCreateConfig.secrets[]` with create-time assignment,
  sentinel minting, and env injection; `ResolveSandboxSecret` resolve-by-sentinel
  entry point building on the `SecretRequest` approval flow; CLI `--secret/-s`
  (`KEY=VALUE` inline, `KEY=<ID>` reference) plus fuzzy `--env` secret detection
  (`KEY`/`TOKEN`/`PASS`/`SECRET`) with `KEY!=VALUE` override.
- [x] Phase 3 — wiring: the `ResolveSandboxSecret` API operation for pool
  principals (`server/internal/handlers/secrets.go`); the pool-agent
  `secrets.Resolver` implementation calling it (`pool-agent/proxyagent/secrets.go`);
  the create request carries the sandbox's sentinel set, which
  `sandboxruntime.CreateSandbox` writes with `proxyagent.UpsertSandboxSentinels`
  to the secrets file the proxy watches into `Config.Secrets`; anonymous secrets
  and assignments are removed on sandbox delete (`deleteSandboxSecretsTx`); `run`
  shares the `--secret`/fuzzy `--env` handling
  (`sandboxcreate.EnvAndSecretsFromOptions`).
