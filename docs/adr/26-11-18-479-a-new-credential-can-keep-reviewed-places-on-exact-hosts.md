# 26-11-18-479 — A new credential can keep reviewed places on exact hosts

- **Status**: Accepted
- **Date**: 2026-11-18
- Accepted by the owner by merging #1396, 2026-11-19.
- **Issue**: [#1394](https://github.com/discobox-ai/discobox/issues/1394)
- **Amends**: [26-11-11-857](26-11-11-857-a-credential-stored-to-answer-a-request-is-held-to-the-requests-hosts.md)
  for the stored credential's body and WebSocket permissions.

## Context

On main at `3ad1f8446`, the console initializes a new credential with a
bare route for the exact, open hosts named by the request
(`cli/internal/tui/credential_request_steps.go`, `newCredentialHosts`).
The final review compares the selected grant routes with the stored routes
it will write (`approvedRoutes`, `approvalConsequences`). Bare host routes
cover headers and query, and the server rejects a binding that names a body
place its credential cannot cover
(`server/internal/resources/secrets/bindings.go`, `checkSecretBodyPlaces`).
The immediate grant and the stored credential are separate route lists
that both have to allow substitution. Explicit WebSocket message places
follow that same rule under [26-11-06-442](26-11-06-442-websocket-credentials-require-explicit-message-places.md).
The WebSocket case depends on #1297, which is open at the time of writing.

## Decision

### 1. Save places with explicit consent

After the approver chooses how to answer each part, offer to store the
credential with the eligible body and WebSocket places in that answer.
Keep the bare exact-host route for header and query reuse. The final review
shows the routes that will persist, including methods, paths, body pointers
and WebSocket message predicates where applicable, separately from the
immediate use and delegation grants and their lifetime. Saving places
requires an explicit affirmative choice. The consent applies to the
displayed routes and must be renewed after edits that change them.

### 2. Derive places from the final decisions

Use the final route list of each approved use or delegation part. An
as-requested approval takes that part's routes. An edited approval takes
the edited routes, subject to the original request's exact-host ceiling.
Denied and absent parts contribute nothing. Host-only and whole-credential
scopes contribute no places of their own. A whole-credential grant uses the
stored credential's resulting routes, including any places explicitly
saved from another approved part. Show that effective permission before
the approver confirms the answer.

Keep explicit placement routes only for literal hosts on those routes that
are also in the original request's open exact-host set. A wildcard entry
contributes no places, even if another route named a host the pattern
matches. A mixed host list can contribute its eligible literals. Preserve
the normalized route's placement restrictions, including the method, path,
protocol and message predicates. Honor applicable route exclusions so
the stored placement permissions from each part are contained by that
part's final routes on those exact hosts. Omit a candidate whose containment
cannot be established, and disclose the gap. A blocked route never
contributes an allow. Rules for selecting compatible existing credentials
and authorizing grants on the server still apply.

### 3. Resolve gaps before storing

The immediate grant keeps its separately approved scope, addition or
replacement mode, and lifetime. The stored credential's ceiling continues
to apply to it and to later grants. The console never removes or rewrites
a requested grant in order to submit it. If the approved placement routes
are not fully covered, the final review identifies the gaps and offers to
return to the part decisions or to use the Secrets screen (F4) to store a
credential with explicitly chosen routes, then resume with that credential.
The same options apply if the approver declines to save places and the
answer needs them. Requests with no open exact host continue to use F4.
An answer denying every part stores no credential.

Keep the existing store-then-approve order. A successful store followed by
an approval refusal leaves the credential with the routes the person
consented to save. Report that outcome and let the person select the saved
credential when retrying, subject to current server validation.

## Alternatives rejected

- **Store every requested route automatically.** A request's unedited
  routes may include a denied part, a broader destination than the
  approver chose, or a wildcard. Persisting those routes would let them
  affect later grants without a corresponding approval.
- **Save places by host alone.** The same pointer can appear in operations
  that serve different purposes. Dropping a method, path or message
  predicate would allow substitutions the person never reviewed.
- **Keep F4 as the only route editor for this case.** It preserves a
  deliberate save but requires entering the credential in a separate flow
  even when the routes already reviewed are sufficient.

## Consequences

Exact-host requests with supported placements can be satisfied in one
pass. A later grant can reuse the consented placements, and any new
placements require another explicit credential edit. The layout of the
consent and the representation of the derived route list are implementation
choices as long as the review and write enforce the same ceiling.
