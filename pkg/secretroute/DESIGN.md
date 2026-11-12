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
