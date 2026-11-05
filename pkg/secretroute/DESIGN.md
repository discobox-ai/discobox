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
restrictions preserve the matcher's unknown-body refusals. Grant-side blocks
can only remove requests. Changed parent lists containing blocks fail closed;
no sampling or independent comparison of negative routes is used.

Each list is bounded to 128 routes, 256 patterns and 64 KiB of formatted text;
at most 4096 route-pair comparisons run. Unsupported proofs return an explicit
refusal requesting provably narrower routes or a person's grant. Omitted binding
lists inherit and are handled by the caller's effective-parent binding.
