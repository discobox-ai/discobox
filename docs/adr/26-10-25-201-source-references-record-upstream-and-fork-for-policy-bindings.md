# 26-10-25-201 — Source references record upstream and fork for policy bindings

- **Status**: Rejected by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-10-25
- **Supersedes**, once accepted:
  [26-10-20-782](26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md)
  §4's definition of `$primary.upstream` as GitHub's observed parent, and
  §§4–5's requirement that this source relationship come from an upstream
  response observed by the control plane. Other observed authorization facts
  retain §5's provenance and freshness requirements.

## Context

A development box may be cloned from an upstream repository while its changes
are pushed to a personal fork. A reusable access policy needs to name both
without hardcoding an owner or repository. `$primary.repo` already names the
cloned repository; `$primary.upstream` is reserved but does not resolve today,
and there is no `$primary.fork`.

The creating client can discover these relationships while preparing the
source. They describe the repositories selected for the work, not the identity
of the credential eventually swapped into a request. Resolving them from that
credential would make the source's policy targets depend on which credential
was selected.

## Decision

### 1. Upstream and fork belong directly to the source reference

A source reference carries optional `upstream` and `fork` repository URLs,
alongside its existing repository information. They are not GitHub-specific
nested metadata and are not fields on the sandbox independent of its sources.

- `upstream` names the root repository of the fork network, whether the source
  was cloned from that root or a fork. It does not mean a local Git remote
  merely named `upstream`, or the branch's tracking remote.
- `fork` names the selected development fork in that network. A source cloned
  from the selected fork may therefore name that same repository as `fork`.
- The source's existing repository URL continues to name what was cloned;
  neither field changes source delivery, cache identity, or checkout selection.

For example, a source cloned from upstream may additionally carry:

```json
{
  "upstream": "https://github.com/discobox-ai/discobox",
  "fork": "https://github.com/ibuildthecloud/discobox"
}
```

No account ID, username, credential reference, or provider-specific identity
section is stored with these relationships. Credential identity remains with
the credential being exchanged.

### 2. The client resolves the relationships at creation

The creating client discovers the upstream and selected fork through the
forge while preparing each source, and sends their URLs in the sandbox create
request. It verifies the relationship rather than constructing a fork URL
from an account name and the upstream's basename; a fork may be renamed.

Discovery uses the client's selected forge account. An ambiguous choice of
fork requires explicit selection rather than guessing. An unavailable lookup
or absent relationship leaves the corresponding field absent; discovery does
not create a fork or prevent otherwise valid source creation. A client without
forge discovery support may omit both fields.

The server validates repository URL syntax and persists these fields with the
source at creation. They are client-reported creation metadata, not an
independent server attestation of a live forge relationship. A sandbox cannot
change its policy targets by editing local remotes or reporting new facts.

### 3. Policy variables read the stored source snapshot

`Sandbox.PolicyFacts()` exposes the stored relationships as typed source
facts. `$primary.upstream` and the new `$primary.fork` resolve to their
normalized repository paths, using the same repository normalization as
`$primary.repo`. URLs retain the host in the source record; credential host
restrictions remain necessary just as for `$primary.repo`.

The facts endpoint, approval, delegation, and runtime binding all derive from
the same persisted source facts. No forge lookup runs during approval or an
authorization decision for these variables. Approval displays the concrete
repositories, and the policy records their resolved values. Runtime binding
requires those same values, as today. Missing fields match nothing.

These variables authorize repository targets selected at creation; they do
not prove that the fork relationship still exists or that the current
credential owns the fork. Replacing a credential changes the actor, not the
approved repository targets. The forge still enforces that actor's access.

### 4. Existing records remain valid

The fields are optional. Existing sources and sandboxes retain their data and
have no relationship values unless recorded at creation; there is no inferred
backfill from repository names or local Git configuration. A grant whose
variable previously resolved to nothing does not acquire access merely because
a relationship later becomes available: existing approval binding rules apply.

## Alternatives rejected

- **Resolve in the server using the credential selected for a grant.** This
  couples stable source targets to credential identity, introduces forge
  access into facts resolution, and requires credential context in today's
  sandbox-only facts API.
- **Store a GitHub username and construct its fork path.** Credentials already
  own identity; renamed forks and accounts without a fork make constructed
  paths unreliable. An owner-wide permission would also exceed one source.
- **Use a GitHub-specific section or sandbox-level relationship fields.** The
  data describes each source, and URLs express it without making the stored
  contract specific to one forge.
- **Require live server verification of these source bindings.** A binding
  selects a concrete repository for approval; it is not evidence used to
  establish ownership or current fork membership. Live verification adds
  credential and availability dependencies without changing the approved
  target. Other authorization facts still use ADR 26-10-20-782 §5.

## Consequences

Reusable policies can read upstream, push to a selected fork, and target
upstream pull requests without spelling either repository. The approval UI
must show the resolved repositories and must not present client-reported
relationships as independently verified observations.

The API source contract and persisted source model gain optional URLs, the
client gains discovery, and policy facts gain source relationship fields and
`$primary.fork`. Current-state design documents change with implementation,
not with this proposal.

## Deferred

Updating relationship metadata on an existing sandbox is outside this decision.
Revisit when users need to select or create a fork after sandbox creation;
that design must preserve approval binding and require fresh approval for a
changed target.
