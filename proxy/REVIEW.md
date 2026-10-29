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
- A new spool kind counts toward the spool budget (`Recorder.EnforceBudget`),
  reports its writes to the running total, and defines a truncation point that
  leaves it readable, as `streamCut` does for streams.
- A budget pass must never change a spool file's modification time, delete a
  row, or touch an open spool. The age sweep pairs files with rows by mtime.
- Do not spool a response body the cache stores or serves. The cache entry is
  the record (`BodyFormatCache`), and reading it must not touch the cache's LRU.
- Do not reclaim audit data on sandbox deletion. The trail deliberately outlives
  the sandbox; retention and the spool budget are all that remove it.
- Do not sweep the response cache by age. Its entries are content-addressed and
  bounded by the LRU byte ceiling; anything that puts a file in the cache
  directory must be describable by the index, or it leaks past that ceiling.
- Preserve mTLS as the pool host proxy client identity boundary. Do not accept
  client identity from sandbox-supplied headers.
- Anything in a sandbox that reaches a pool service dials through
  `bridge.Dialer`, never a `tls.Dial` over `"tcp"` of its own: the URL's scheme
  picks the transport (`wire`), and a sandbox in a host-VM pool has no network
  to dial TCP over (ADR 0144 §4). The transport changes; the mTLS on top never
  does.
- Behind an edge, identity still comes only from the client certificate inside
  the tunnel. Never authenticate a sandbox by a header on the upgrade request,
  signed or set by the edge (ADR 26-10-23-719): the edge reads and can replay
  anything outside the inner TLS.
- A `wstunnel` tunnel carries one stream at a time. Do not multiplex streams
  onto one; concurrent streams take concurrent tunnels, so no stream shares
  another's congestion window.
- Policy and audit behavior must remain client-scoped. Destination rules,
  gateways, upgraded-stream metadata, and audit query surfaces
  must preserve the authenticated client ID.
- Runtime policy changes go through `ApplyConfig` only; do not add an HTTP
  config API to the proxy without revisiting the design.
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
- Redact credential-like headers and every header name a gateway touches.
  Never persist a gateway's header values in audit rows.
- Keep certificate preparation callable without starting the proxy listener.
- A gateway rewrites only the copy `roundTrip` sends. Nothing before it — the
  filter, the swap, the authorizer, the cache key, the audit row, a credential
  report — may see the gateway's URL or headers: they judge and record the
  request as the sandbox sent it (ADR 26-10-16-470 §3).
- Never send a gateway's request without a header it takes from the server's
  environment, and never fall back to the original host. Unset is a 502; no
  answer is a 503.
- Anything the proxy asks the control plane for on the request path follows
  ADR 26-10-14-482's outage rules — retry within the window
  (`secrets.RetryTransient`), serve what is held, then a 503 with
  `X-Should-Retry` — or a server restart fails every such request.
- A gateway matches the host and path the way policy and the upstream read
  them: no trailing dot, unreserved escapes decoded (`/%761` is `/v1`), dot
  and empty segments resolved, the cleaned path forwarded, an encoded slash
  refused. Matching the raw URL lets a sandbox
  step around a gateway or out of its target's prefix with the gateway's key.
- A 401 is evidence about a swapped credential only if the credential reached
  the upstream. A gateway that removes or sets the header it was swapped into
  makes the answer the gateway's: never retry or report it.
- Avoid importing server internals. The package lives in the root module and
  is consumed through root-module contracts: `pool-agent/proxyagent` runs the
  pool proxy, and sandbox-agent imports only `proxy/bridge`.
- Never persist a swapped secret value. Any header whose value was
  secret-swapped must be added to the audit redaction set, and a query-param
  swap must record the pre-swap URL. Real values must never reach an audit row,
  spool, or cache key.
- Sentinel detection is exact-set matching over a client's configured sentinel
  strings; do not add prefix/format heuristics that could misclassify real
  tokens. Sentinels are non-secret; the real value comes only from the injected
  `secrets.Resolver`.
- Base64 is a transport encoding, not an exception to that: a token is decoded,
  matched against the same sentinel set, and rewritten only when a sentinel
  resolved. Re-encode with the encoding the token arrived in — a value that
  comes back re-padded or in the other alphabet is a corrupted request. Never
  rewrite a token on anything weaker than a sentinel match.
- `tokenEncoding` must keep the property that the encoding it picks re-encodes
  the token it was given to itself, so passing a token through cannot reshape
  it. Its ambiguous case (a multiple of 4, no `=`, no character 62/63) is a
  documented default, not an inference; changing which way it defaults changes
  what `Authorization: Basic` receives.
- Any new place a value is scanned must go through `swapSentinels`, so the
  literal and base64 forms stay one behavior. `Apply` and `ApplyPrevious` share
  it deliberately; a second hand-written scan loop is how one of them silently
  stops covering an encoding.
- The resolver is a stable construction dependency, not reloadable config. It
  must be preserved across `ApplyConfig`; only the sentinel set and swap tuning
  come from `Config.Secrets`.
- On resolver denial or pending approval, leave the sentinel in place
  (fail-closed on the secret) and send the request. On a transient resolver
  error, never send it: retry within the window, then refuse the whole request
  with a `503` (ADR 26-10-14-482 §2). A raw sentinel reaching an upstream
  because the control plane blinked is the bug that ADR fixes; no path —
  first attempt or the `401` retry — may send one for that reason.
- Keep one `Swapper` for the life of the proxy. `ApplyConfig` reconfigures it
  in place (`Swapper.Reconfigure`); building a new one empties the resolve
  cache on every trust refresh, and a control-plane outage then finds nothing
  to serve (ADR 26-10-14-482 §1). Anything cached for a `(client, sentinel)`
  that leaves the set is dropped at that publish, and a late answer for it is
  not stored.
- A 401 on a *swapped* request is retried once, and only ever with a credential
  that differs from the one just rejected. Never retry the same value, never
  retry more than once, and never retry a request whose body was too large to
  hold — a retry must not become a way to duplicate or amplify upstream load.
- Never read a request body of undeclared length ahead of sending it. Over
  HTTP/2 every body is a stream, and a bidirectional gRPC call waits on the
  response before it sends more; anything that needs the body whole copies it
  as it passes, as the retry does.
- Whatever reads a request body before it is sent — the route body
  (`routeBody`) — puts back exactly what it read in front of the rest, and is
  bounded before and after decoding. A body a protocol route cannot read
  answers false, never an empty or partial body: an open route must not match
  what nobody read.
- A rejection is reported off the request path, never on it, and never more
  than once per `(client, sentinel, host)` per cooldown. Reporting state belongs
  to the proxy, not the `Swapper`: the `Swapper` forgets a sentinel that leaves
  its client's set, and a rejection's cooldown must outlive that.
- A report names a sentinel, a client and a host. Never put a resolved value, or
  anything derived from one, into a report.
- A queue the datapath writes to and shutdown closes needs the close guard, not
  just the bounded drop: `Server.Close` stops waiting for in-flight connections
  after 30s, so an enqueue after the close is reachable and would panic the
  proxy rather than drop.
- Only a 2xx retracts a rejection. A 3xx is frequently how an upstream says a
  session is dead.
- Only a request this proxy swapped a credential into may be reported. A 401 on
  a credential the sandbox brought itself is not evidence about a secret.
- Keep the previous-value memory out of the cache entry. `Invalidate` drops the
  entry, and the fallback exists precisely to outlive the value being dropped.
- Do not scan or swap request bodies while the request-body audit spool would
  capture the swapped value.
- A response that cannot carry a body (HEAD, 1xx, 204, 304; see
  `bodyAllowed`) leaves the handler with `http.NoBody` and nothing wrapped
  around it — on the upstream path and on a cache hit alike (`discardBody`).
  goproxy's MITM loop frames any other body as chunked, an empty wrapper
  included. A 1xx, 204, or 304 also loses its transfer coding, since goproxy
  writes a chunk terminator for one still marked chunked, and the terminator
  stays on the kept-alive connection for the client to read as the next
  response. A HEAD keeps its transfer coding: it describes the GET, and
  without it net/http closes the connection after an unsized HEAD.
