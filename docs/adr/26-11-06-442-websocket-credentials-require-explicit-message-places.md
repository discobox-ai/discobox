# 26-11-06-442 — WebSocket credentials require explicit message places

- **Status**: Proposed (would narrow ADR 26-10-27-880 §4's unreadable upgraded bodies and absent-protocol-block permission for WebSocket messages; extend ADR 26-11-04-389 §2's containment proof; apply ADR 26-10-14-482 §§1–3 to message resolution)
- **Date**: 2026-11-06
- **Issue**: [#741](https://github.com/discobox-ai/discobox/issues/741)

## Context

Checked against `main` at `00541f9c90fefeabe1b61134a737fab287861803`:

- `proxy/http.go`, `upgradedResponseStream.Write`, captures client bytes then
  writes them unchanged. `proxy/internal/audit/websocket.go` already unmasks,
  reassembles and decompresses messages, but is an observer, not enforcement:
  capture failure stops recording rather than forwarding.
- `pkg/secretroute/secretroute.go` has GraphQL and MCP blocks. Its absent-block
  rule permits any HTTP body. `decide.go` ranks candidates by host then path,
  lets a tied block win, and intersects every effective route list.
- `proxy/internal/secrets/secrets.go` separates authorization from resolution;
  authorization precedes even cached values. `Reconfigure` preserves usable
  cache entries while removing departed sentinels.
- `pkg/secretroute/contain.go` proves bounded restrictions of open parent lists
  or normalized equality, conservatively refusing other blocked-parent proofs.
- `pkg/credentialpolicy/policy.go` substitutes `pathSegment` parameters only in
  URL paths. Policies are concrete routes, not additional authority.

Discord Gateway authentication puts a token in `/d/token` in IDENTIFY (`op: 2`)
and RESUME (`op: 6`), after the upgrade. An HTTP-only sentinel cannot work
there. Replacing it everywhere on the connection would disclose the token in
an activity name or another field the service publishes. Permission must name
where the application consumes a credential, not merely its transport host.

## Decision

### 1. An explicit WebSocket block grants places, not a connection

Add `websocket` as a third, mutually exclusive protocol block on a route:

```json
{
  "hosts": ["gateway.discord.gg", "*.discord.gg"],
  "methods": ["GET"],
  "websocket": {
    "places": [
      {"at": "/d/token", "when": {"op": 2}},
      {"at": "/d/token", "when": {"op": 6}}
    ]
  }
}
```

Hosts, paths and all effective methods refer to the original upgrade request,
not a gateway rewrite. The resumed connection is authorized against its own
host; a host supplied by an origin confers no permission. The handshake still
passes existing network policy and HTTP credential checks. A WebSocket block
is eligible only for a valid HTTP/1.1, version-13 WebSocket upgrade; it grants
no ordinary HTTP body permission. Handshake credentials retain their existing
HTTP rules and are not implicitly authorized by message places.

`places` is a nonempty finite set. Each `at` is a nonempty RFC 6901 JSON
pointer, without URI-fragment syntax, wildcards or append (`-`) array indexes.
Validate `~0` and `~1` escapes; preserve member names exactly. Array indexes
are canonical nonnegative decimal integers (no leading zero except `0`).
The pointed-to value must exist and be a string exactly equal to one of this
client's configured sentinels. Substrings and base64 wrappers are not eligible
for replacement.

`when` is an optional conjunction of exact, case-sensitive top-level member
names and JSON scalar values (string, number, boolean, null). Missing differs
from null; arrays and objects are invalid conditions. Empty or absent `when`
is unconditional. Strings compare decoded Unicode scalar sequences without
normalization; numbers compare exact decimal mathematical values, so `2` and
`2.0` are equal, without floating-point rounding. Reject invalid UTF-8,
unpaired surrogate escapes and duplicate object members at every depth after
key unescaping. A message must be exactly one JSON object, with only JSON
whitespace around it. No case folding, JSONPath or application expressions.
All conditions are evaluated on the original, unsubstituted message.

The typed spelling is `ws=` followed by the JSON array of places:

```text
GET gateway.discord.gg,*.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6}}]
```

Parse the array as a JSON value, including whitespace within it, rather than
splitting it into words or comma-delimited expressions. The existing final
`=blocked` suffix follows the complete array. `Format` emits compact JSON,
sorted condition keys and sorted, deduplicated places; normalize numeric
condition values for stable equality. Existing typed routes remain valid.
Schemas and strict policy parsing accept this block. Policy parameters remain
URL-path-only: placeholders in `at` or `when` are refused. Extending parameter
types and escaping is deferred until a concrete service requires configurable
message places; the Discord grant needs none.

### 2. Every effective list must explicitly allow every occurrence

An absent WebSocket block grants **no message substitution**, including on a
route naming `*`. This deliberately narrows ADR 26-10-27-880 §4 for messages;
its ordinary HTTP absent-block behavior remains unchanged. An omitted binding
list still inherits, rather than creating a new empty restriction.

For each sentinel occurrence, each effective list must select an allowed
place at that exact pointer whose conditions hold. Within a list, retain
host-then-path specificity and tied-block precedence. Open routes without a
WebSocket block are not message candidates. Blocked routes without a protocol
block remain broad carve-outs on matching upgrade coordinates; blocked
WebSocket routes match the occurrence's pointer and conditions. GraphQL/MCP
routes cannot grant message use. Conditions do not add a specificity rank.
A list may use different places to authorize different occurrences, but lists
never combine to cover an occurrence that one of them refuses. Preserve
`DecideGrants` (ADR 26-11-03-329 §3): for each sentinel in one message,
one complete live grant must allow all its occurrences through all its
lists. Separate grants permitting `/a` and `/b` cannot authorize that
sentinel at both pointers in one message. Another complete live grant may
allow the message; different sentinels may use different grants.

Before resolving anything, examine the whole message for every configured
sentinel occurrence, including substrings, decoded JSON strings and member
names. Recognizable base64-wrapped sentinels use the existing scanner's
encoding rules for refusal, never substitution. A sentinel outside an allowed
exact string value refuses the entire message, even if another occurrence is
allowed. Do not resolve or forward a prefix of a message that will be refused.
Non-JSON text and binary messages have no eligible places; scan their complete
unmasked payloads for literal and supported base64 sentinel forms and refuse
any found. No promise is made to recognize arbitrary application encodings.

Containment now covers HTTP permission and message permission separately.
For open message grants, the child pointer must equal a parent pointer and
its condition conjunction must imply the parent's: every parent condition
appears with an equal scalar in the child. Extra child conditions narrow it;
removing a condition widens it. Each child place must fit one parent place.
A parent without a block contains no nonempty child message grant, even
though it allows ordinary HTTP bodies. Conversely, a route without a block
is not automatically contained by a WebSocket-only parent: its HTTP authority
must also fit. Retain normalized equality, blocked-parent conservatism and
runtime list intersection from ADR 26-11-04-389 §2. Charge pointer and condition
counts and bytes against existing route and proof budgets before comparison.

### 3. Inspect bounded complete messages before sending data

Select the inspecting path before forwarding the upgrade if any open
WebSocket route for this client's sentinels matches the upgrade coordinates
in any effective list. It need not already pass every list: incomplete
intersection must refuse use, not silently bypass inspection. Connections
with no such candidate retain today's byte-preserving path and capture cost.
They gain no message substitution permission.

For inspected connections, remove **all** WebSocket extension offers from the
upstream handshake. Validate the response before relaying 101 and reject an
unsolicited extension. This includes `permessage-deflate`: no compression or
context takeover occurs in either direction. Preserve subprotocol negotiation.
A service requiring an extension cannot use this grant in this version.

Hold at most 64 KiB of client application payload per complete message, across
fragments. Refuse **every** larger message with close 1009, whether or not it
contains a sentinel; do not switch to passthrough or scan masked wire bytes.
Check declared frame lengths before allocating. Also limit each application
message to 1024 data/continuation frames and 80 KiB of held wire bytes, with
bounded per-frame metadata; empty fragments count. Exceeding any of these
limits closes with 1009. In addition, the aggregate queued-plus-assembling
work for the connection is limited to 64 KiB application payload, 80 KiB
retained wire bytes and 1024 data/continuation frames, with bounded metadata
per frame and per queued message. Empty complete messages consume these
frame and wire budgets too. Never reset aggregate counters when a message
is enqueued; release its charge only when it is sent or discarded. Bound
parsing depth to 64
containers and queued application payload to the same 64 KiB; backpressure
stops reading later data when the queue is full. Input parsing and output
serialization both enforce the 64 KiB limit, including growth after replacement.
A replacement that exceeds it is refused whole with 1009.

Validate RFC 6455 framing, masking, continuation ordering and reserved bits;
protocol errors close with 1002 and invalid text UTF-8 with 1007. Fragmented
messages are unmasked and reassembled before policy evaluation. Valid ping,
pong and close frames remain serviceable during fragmentation and bounded
resolution waits, with serialized writes in each direction. A bounded queue
may apply backpressure; servicing control frames cannot require unbounded
read-ahead past application data. Close cancels held work. Control payloads
are never substituted; a recognizable sentinel there is refused with 1008.

After authorization, resolve all eligible sentinels using the existing
resolver and value cache. Re-serialize changed string values with proper JSON
escaping, then send a single text frame with a fresh cryptographic mask.
Unchanged complete messages may retain their original frames after inspection.
No application data from a held message reaches the origin early. Successful
server-to-client traffic is not rewritten; capture retains its current
redaction. Frame parsing belongs to enforcement even if audit capture is off,
full or broken; share mechanics where useful, never its fail-open lifecycle.

| Outcome | Forwarding and audit |
| --- | --- |
| Every occurrence allowed and resolved | Send substituted message; capture the original sentinel message. |
| Any occurrence outside permission | Send no message; close 1008; one blocked audit event with a bounded `route:` reason. |
| Authoritative resolve denial or pending approval | Leave that allowed sentinel in place; send after the whole message passes all other checks. |
| Transient authorization/resolution failure with no usable answer | Hold the entire message within one `ResolveRetrySeconds` window, then send none and close 1011; audit once as blocked. |
| Oversize or malformed framing/text | Send no affected application message; close with the code above and audit a bounded reason. |

The retry window is per message, shared across sentinels and authorization,
not multiplied by their count. Preserve ADR 26-10-14-482's cache hard expiry
and authoritative-denial distinction. Do not replay an authentication message
or infer credential rejection from application close codes: the HTTP 401
rotation retry does not apply to WebSocket messages.

### 4. A connection does not freeze a grant

Authorize every credential-bearing message against current effective route
lists before consulting cached values. Carry message context through the core
authorization contract, updating every resolver implementation; do not add an
optional interface or call ordinary HTTP authorization with a fabricated body.
`pkg/secretroute` owns the common message decision and containment semantics;
`proxy/internal/secrets` owns matching and substitution with the shared cache.
The proxy owns framing and audit integration.

Use the latest published client configuration and effective binding expiry at
message forwarding. Recheck its revision and expiry after resolution; serialize
the forwarding decision with configuration publication so a stale held result
cannot start sending after revocation. Changes already transmitted cannot be
recalled. Removing a sentinel invalidates its held resolution and cache as in
ADR 26-10-14-482 §1. On inspected connections retain the connection's previously
recognized sentinel strings for refusal only: removal must not turn an old
sentinel into unchecked traffic. New sentinels use current grants. Inspection
never downgrades to opaque copying; opaque existing connections require a new
handshake before gaining message substitution.

This is enforcement at the pool's published configuration boundary, not a
claim of synchronous control-plane revocation. An expired grant cannot be
used because a value is cached. Revocation does not undo an authenticated
session the origin already accepted, and does not terminate credential-free
messages on that session. Stronger upstream session revocation requires a
separate service-specific design.

Audits retain pre-swap messages, never resolved payloads. Redact server echoes
and all new diagnostics through the existing client redactor before spool,
row or log writes. Policy refusals are explicit blocked events, even though
the HTTP handshake succeeded; 101 alone cannot describe the outcome. Preserve
one refusal per failed message/connection, its authenticated client and bounded
reason. Record original payload size and sent wire byte counts distinctly;
never put resolved values in counters, cache keys or reports.

### 5. Boundaries and acceptance evidence

This grant is JSON text over HTTP/1.1 WebSocket only. Binary protocols (ETF,
msgpack, protobuf), HTTP/2 extended CONNECT and HTTP/3 WebSockets are deferred
until a concrete caller needs them and an equivalent bounded place matcher
is designed. A binary sentinel on an inspected connection is refused, not
silently upgraded to a broader grant.

Server echoes still reach the sandbox unchanged, as HTTP response bodies do
today. Audit redaction is not response protection. Defer bidirectional real-
value-to-sentinel rewriting until a service that echoes authentication values
requires it, or a separate response-protection design is proposed. Approvers
must understand that the allowed service receives the real credential and
may return or disclose it; places restrict where discobox inserts it, not
what that service does afterward.

The API route schema, generated types, conversions, policy editor schema,
CLI typed parsing and approval displays must preserve places end to end.
Approvals show the host/path, exact pointer and all conditions, in plain
words where possible, and explicitly show unconditional places. Old clients
must not silently strip an unknown block and store a broader route.
Implementation must establish this compatibility behavior before enabling
writes; version rejection or strict unknown-field refusal is acceptable,
silent loss is not.

Acceptance uses a local fake WebSocket origin and fake credentials: IDENTIFY
and RESUME succeed; Presence and unauthorized duplicate occurrences never
reach it; each effective list, blocked exception and containment constraint
is enforced. Exercise escaped keys/strings, numeric equality, duplicate keys,
invalid Unicode, typed round trips, bounded policy/proof input, malformed and
binary payloads, masked fragments, split sentinels, oversized input/output,
empty-fragment and queued-empty-message floods, control frames and extension refusal. Exercise refusal
when only separate grants cover different occurrences, success through one
complete alternate grant, and expiry of that alternate grant. Observe
denial/pending versus transient
failure, cached values through an outage, expiry/revocation during resolution,
sentinel removal, and opaque-connection compatibility. Audit artifacts and
refusal events must contain no real credential, including origin echoes.
These are implementation acceptance requirements, not claims of verification
by this documentation-only proposal.

## Alternatives rejected

- **Scan and swap anywhere on an allowed connection.** A service can publish
  an unrelated field, disclosing the inserted token to other users.
- **Treat an absent block as unrestricted messages.** Existing broad host
  routes would gain application-level credential authority without consent.
- **Stream oversized messages and scan wire chunks.** Masking, fragmentation
  and JSON escapes defeat that scan; a prefix may escape before a refusal.
- **Reuse capture as the enforcement gate.** Capture deliberately stops on
  budget or storage failure while traffic continues; permission cannot do so.
- **Negotiate compression and re-compress changed messages.** Context takeover
  couples later messages to the rewritten history, increasing state and failure
  modes without being needed for the motivating authentication messages.
- **Freeze authorization at the upgrade.** Long-lived sockets would outlive
  binding expiry and narrowing despite checking every HTTP request today.
- **Add `/d/token` as a Discord-specific rule.** It embeds service knowledge
  in the proxy and cannot express another service's authentication message.

## Consequences

A grant can authorize Gateway IDENTIFY and RESUME without exposing a real token
inside the box under normal Discord behavior. The person granting it must
still trust the allowed service and the semantic meaning of its named places.
Existing routes acquire no message substitution rights. Inspected connections
pay bounded buffering and lose extensions; unrelated connections retain their
wire behavior. Accepting this proposal requires no implementation in this PR;
current-state design docs change with the implementation.
