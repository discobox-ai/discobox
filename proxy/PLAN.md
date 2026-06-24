# Proxy Implementation Backlog

Track proxy-component work here until the worker-agent/sandbox-agent integration
phase starts. This list intentionally excludes container/image wiring.

## Work Items

- [x] Add OpenTelemetry spans on the core datapath: mTLS accept, protocol
  detection, HTTP request handling, CONNECT/MITM, header rewrite, cache
  lookup/store, audit enqueue/drop/write, and SOCKS connect handling.
- [x] Add sandbox-local forwarding proxy mode that accepts localhost proxy traffic
  and forwards to the worker proxy with the sandbox client certificate.
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
