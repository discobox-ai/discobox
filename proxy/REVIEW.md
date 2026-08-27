# Proxy Review Notes

- Do not put payload blobs in SQLite. Keep cache assets and captured HTTP
  request/response bodies in disk files, and store only audit/cache/spool
  metadata in the database.
- The proxied request path must never synchronously wait on SQLite audit writes.
  New audit data must go through the bounded async recorder or an equivalent
  non-blocking spool.
- Audit rows and their spool files must stay reclaimable together. A new spool
  kind must be swept by `Recorder.Sweep`, and anything that holds one open past
  the retention window must be tracked open so the sweep skips it.
- Do not reclaim audit data on sandbox deletion. The trail deliberately outlives
  the sandbox; retention is the only thing that removes it.
- Do not sweep the response cache by age. Its entries are content-addressed and
  bounded by the LRU byte ceiling; anything that puts a file in the cache
  directory must be describable by the index, or it leaks past that ceiling.
- Preserve mTLS as the pool host proxy client identity boundary. Do not accept
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
- Avoid importing server internals; this package is consumed by pool-agent
  and launch wiring through root-module contracts.
- Never persist a swapped secret value. Any header whose value was
  secret-swapped must be added to the audit redaction set, and a query-param
  swap must record the pre-swap URL. Real values must never reach an audit row,
  spool, or cache key.
- Sentinel detection is exact-set matching over a client's configured sentinel
  strings; do not add prefix/format heuristics that could misclassify real
  tokens. Sentinels are non-secret; the real value comes only from the injected
  `secrets.Resolver`.
- The resolver is a stable construction dependency, not reloadable config. It
  must be preserved across `ApplyConfig`; only the sentinel set and swap tuning
  come from `Config.Secrets`.
- On resolver denial, pending approval, or error, leave the sentinel in place
  (fail-closed on the secret). Do not swap partially or block the request.
- A 401 on a *swapped* request is retried once, and only ever with a credential
  that differs from the one just rejected. Never retry the same value, never
  retry more than once, and never retry a request whose body was too large to
  hold — a retry must not become a way to duplicate or amplify upstream load.
- Keep the previous-value memory out of the cache entry. `Invalidate` drops the
  entry, and the fallback exists precisely to outlive the value being dropped.
- Do not scan or swap request bodies while the request-body audit spool would
  capture the swapped value.
