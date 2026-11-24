# Route containment

`ProveContained` is the bounded write-time proof for sandbox handoffs
(ADR 26-11-04-389). Runtime `Decide` intersection remains mandatory.

Normalize, sort and deduplicate each list. Identical normalized lists are equal,
including specificity, blocked exceptions, unreadable bodies and method overrides.
A limit identical to the secret adds no restriction to the proposed grant.
Otherwise every open grant route must fit one open parent route and the parent
must contain no blocks. Hosts use exact/suffix/any containment, methods set
containment, paths segment-glob containment, and protocol names prefix containment.
Open matching requires every method, operation or tool to fit, so these
restrictions preserve the matcher's unknown-body refusals. A body block must
be on both sides or neither, and every place the grant names must be one the
parent names: a body route grants places and no header use, so neither kind
of route contains the other. Grant-side blocks
can only remove requests. Changed parent lists containing blocks fail closed;
no sampling or independent comparison of negative routes is used.

Each list accepts the credential-request bounds (512 routes and 256 KiB of
formatted text), with at most 16384 input patterns. Normalization deduplicates
shared policy routes before proof work is budgeted. Identical lists need no
pairwise search; otherwise the product of the two lists' work weights
is bounded to 67108864. Each weight counts routes, patterns and pattern bytes,
so it bounds route-pair traversal, string comparisons and repeated path splitting while allowing a role from a multi-policy delegation. Unsupported proofs return an explicit
refusal requesting provably narrower routes or a person's grant. Omitted binding
lists inherit and are handled by the caller's effective-parent binding.

# WebSocket message places

A `websocket` block (ADR 26-11-06-442) grants places in the client's JSON text
messages on the connection a route's request upgrades: an exact RFC 6901
pointer, and optional top-level conditions compared as exact scalars. It is
the one block whose absence grants nothing: an open route without one,
`*` included, is no message candidate. `Request.Message` carries a parsed
message and `At` its pointers; candidates are a WebSocket route whose place
allows the pointer in that message, or a blocked route without a protocol
block. Specificity is host then path as for every use; conditions add none.

`ParseMessage` reads one strict JSON object (`ParseDocument` any value): no duplicate members, no
unpaired surrogates, depth 64, with numbers kept as exact decimals
(`canonicalNumber`), so `2` and `2.0` are one value and no float rounding
joins two. Conditions are stored in that canonical spelling, which makes
`Format` and containment comparisons byte equality. It builds no tree.
It reads twice: once to refuse and to count the strings and pointer
steps, then again to record them, into slices of that size, with the
root object's scalars. A string keeps one shared step and builds its
pointer only when asked (`MessageString.At`). So reading costs what it
keeps, not a node per value, a pointer per string, or room for strings a
document turns out not to hold. The typed form is
`ws=` and a JSON array, cut out of the text as one JSON value before the
rest is split into words.

Containment needs a WebSocket block on both sides; each child place matches a
parent pointer and repeats every parent condition. `WebSocketCandidate` tells
the proxy which upgrades to inspect, and `PlacesAllowed` refuses a grant
naming a message place no list could allow.

# Body places a grant can use

`PlacesAllowed` says whether one list can ever allow a grant's body and
message places: each place an open body route of the grant names must be named by an
open body route of the list on an overlapping host and method, since `*`
grants no body place. It answers only for what is certainly disjoint (hosts,
methods); paths and blocks stay `Decide`'s. Callers run it against every list
the grant will be held to.

# Any path

`AnyPath` says whether a route's paths reach every path below the root — no
paths, or one that contains `/*/**` by the same segment-glob containment the
proof uses. It is how a summary of a grant names its breadth (the console's
credential request summary), so breadth follows matching, not spelling.
