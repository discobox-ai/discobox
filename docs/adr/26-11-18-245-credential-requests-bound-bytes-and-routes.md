# 26-11-18-245 — Credential requests bound bytes and routes

- **Status**: Proposed (would supersede the policy-count limit in [26-11-02-022](26-11-02-022-a-credential-request-names-its-policies-and-asks-to-use-and-to-delegate-at-once.md) §2)
- **Date**: 2026-11-18
- **Issue**: [#1370](https://github.com/discobox-ai/discobox/issues/1370)

## Context

The request service counts policy occurrences across the use and delegate
parts before resolving either part
([`agentcredentials.go`](../../server/internal/resources/secrets/agentcredentials.go)).
The in-box CLI checks the same count
([`commands.go`](../../access/commands.go)). Each application of a policy to
a repository counts separately. The Discobox manager requests a use role
and two delegation roles per repository it watches. Its 35-policy request
was refused even though both parts fit within their route limits.

The approval view already groups applications of the same document by digest
within each part and shows their arguments and resolved routes together
([`requestrole.go`](../../cli/internal/requestrole/requestrole.go)).
Repeating a role for another repository adds to its scope summary without
requiring a separate role entry.

## Decision

Remove the limit of 32 policy occurrences across the two parts. Apply no
replacement count limit, including a count of distinct documents. A request
with more than 32 occurrences is eligible for approval when it satisfies
the existing validation and size limits.

Keep the 1 MiB body limit and the 64 KiB limit on each policy document.
The body limit bounds the input the service reads and parses, including all
policy occurrences and their arguments. Keep the per-policy expansion
rules of [26-10-30-584](26-10-30-584-a-credential-policy-file-supplies-a-bindings-routes.md)
and the existing shared 256 KiB resolved-route byte budget
([`protocol.go`](../../pkg/agentcreds/protocol.go)). The server charges that
budget as it resolves policies, bounding expanded routes even when a small
document produces long paths.

Keep the maximum of 512 normalized routes per part. That bounds the number
of route entries an approval can grant regardless of how many documents
supply them. A request exceeding the body or route limits is refused with
the applicable limit named.

## Alternatives considered

- Raising the occurrence cap would postpone the same failure as the manager
  adds repositories. A policy occurrence can supply one route or many,
  so its count is a poor measure of the resulting grant.
- Counting distinct documents would accommodate the manager's repeated
  roles but still reject a request composed of many small, distinct
  documents that fit the input and resolved-route budgets. The approval
  view already groups repeated documents and shows the scope of each role.

## Consequences

The manager can request the same roles for additional repositories until
it reaches an existing byte or route limit. Approval and delegation checks
continue to use the resolved routes. The CLI help and request documentation
describe the remaining limits and omit the old policy count.
