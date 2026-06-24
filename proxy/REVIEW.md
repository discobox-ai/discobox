# Proxy Review Notes

- Do not put payload blobs in SQLite. Keep cache assets and captured HTTP
  request/response bodies in disk files, and store only audit/cache/spool
  metadata in the database.
- The proxied request path must never synchronously wait on SQLite audit writes.
  New audit data must go through the bounded async recorder or an equivalent
  non-blocking spool.
- Preserve mTLS as the worker proxy client identity boundary. Do not accept
  client identity from sandbox-supplied headers.
- Policy and audit behavior must remain client-scoped. Destination rules,
  header injection rules, upgraded-stream metadata, and audit query surfaces
  must preserve the authenticated client ID.
- Runtime policy changes should use `ApplyConfig` or the config file watcher;
  do not add an HTTP config API to the proxy without revisiting the design.
- The control API is read-only. Keep audit and stream retrieval scoped by the
  authenticated client/sandbox ID, and do not serve arbitrary spool paths.
- If `Control.TrustPublicKey` is configured, do not bypass PASETO auth on
  control endpoints. Tokens must use the proxy control audience and `audit:read`
  scope; sandbox-scoped tokens must not read another `client_id`.
- Raw upgraded-stream payloads and normal HTTP bodies belong in disk spool
  files, not SQLite. SQLite rows should store only file metadata, byte counts,
  and drop counters.
- Do not mutate `goproxy` package-level CONNECT actions or CA state. Keep MITM
  actions on the proxy instance.
- Redact credential-like headers and every header name touched by a rewrite
  rule. Never persist injected secret values in audit rows.
- Keep certificate preparation callable without starting the proxy listener.
- Header rewrite rule evaluation must remain deterministic across process runs.
- Avoid importing server internals; this package is consumed by worker-agent
  and launch wiring through root-module contracts.
