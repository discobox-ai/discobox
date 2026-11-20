# 26-11-18-649 — A box's manager publishes versioned skills

- **Status**: Accepted
- **Date**: 2026-11-18
- Accepted by the owner by merging #1391, 2026-11-19
- **Issue**: [#1384](https://github.com/discobox-ai/discobox/issues/1384)
- **Relates to**: [0072](0072-a-repository-ships-skills-that-only-exist-in-a-sandbox.md),
  [26-10-09-395](26-10-09-395-a-sandbox-may-be-created-with-skills.md),
  [26-10-14-294](26-10-14-294-a-skills-repository-is-a-source-its-skills-are-linked-from.md),
  [26-11-01-692](26-11-01-692-a-skills-source-is-checked-out-under-home.md),
  [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md),
  and [26-10-29-316](26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md).

## Context

`cli/internal/sandboxcreate/skills.go` packages local skills as content.
`cli/internal/sandboxcreate/skillssources.go` turns a Git skills argument into
a delivered source. The latter leaves a checkout under the box user's home
that the harness can edit and the box can update if it has upstream access.
`sandbox-agent/terminal/skills.go` copies image, repository and uploaded skills,
then links skills from the delivered checkouts, on the primary terminal's first
launch. Subsequent launches do not reinstall those skills. The server stores the
create-time content on the sandbox manifest and reports its names
(`server/internal/store/sandbox_skills_test.go`).

A manager updating long-lived workers needs one operation that changes the
skills the harness actually discovers. The manager already has access to its
skill sources. Giving every worker equivalent access would widen the trust
boundary and make delivery depend on the worker's network policy.

## Decision

### 1. Write a per-box desired revision through the control plane

The box skills API accepts content and provenance, keyed by skill name.
`discobox skills set BOX --skills DIR` replaces the managed set, `add` upserts
the named skills, and `rm NAME` removes a managed skill. Skills installed by
other mechanisms are outside the scope of these mutations. `ls` lists the
harness-visible Discobox-installed skills with their origin and version where
known, including any desired revision awaiting application. The listing uses
the last reported installed inventory when the box is unavailable. It omits
skill bodies and labels unavailable legacy provenance as unknown.

The server persists the managed set and its revision separately from the
create manifest. The request and resulting managed set both use the
`pkg/sandboxconfig.Skills` limits of 1 MiB decoded and 1,000 files, with its
portable path and name validation. For a directory, reuse the create reader's
link and executable-bit behavior, including skipping nested directory symlinks
and excluding `.git`. A followed Git tree may only resolve links within the
materialized source. The control plane and sandbox agent both validate the
content before installation.

Use the existing box-management authorization for human owners and the live
direct-parent check for leads, including the write-side egress coverage check
in `server/internal/auth/authorizers.go`. Creating a box confers no lasting
authority after release. Recheck authority when accepting each sync. A lead
gets no access to another box's source credentials through this API.

Serialize mutations per box. A write that depends on an earlier revision must
compare against it so a slow sync cannot overwrite a newer push or removal.
The API distinguishes committed intent from confirmed installation. Equivalent
content and provenance are a no-op.

### 2. Push a selected source and converge it in the box

A box can opt into following a directory or
`URL[@REF][//PATH]` at creation or later. Following records the association
and its last published snapshot. The managing client reads the directory or
resolves and fetches the Git ref using its own credentials, packages the
selected directory, and sends the snapshot with its resolved commit (for Git)
and content digest. The initial snapshot is carried with create so the first
harness session sees it. A source-targeted `discobox push` repeats the publication,
and managers that want automatic updates invoke it from their own scheduler
or source-change trigger. The API accepts a snapshot directly from such a
publisher.

A sync replaces the skills from that source as a unit, including removals.
An unreadable source or invalid snapshot leaves the previous revision intact.
The box receives the bounded files. It receives no Git checkout, Git metadata
or credentials for the followed source. Store credential-free provenance and a
stable client-side source identifier. A local directory is resolved on the
manager's machine, never against the server's filesystem. Existing create-time
Git skills checkouts retain their editable-checkout behavior. Following
explicitly opts into snapshot delivery.

For managed skills, push installs the whole new snapshot of the selected
source and removes entries it used to supply. This is the hard-reset
semantics for a skills source: edits in the box do not carry into the
installed snapshot, and no skills checkout is needed.

For a Git work source, offer explicit reset on push. Today
`cli/internal/sandboxpush` only updates a pool ref under a lease, leaving the
working tree alone. Preserve plain push for developers and attached clients
that rely on that behavior. With reset requested, the caller selects the
source and publishes a commit through the pool. The control plane records the
exact delivered commit and the sandbox agent applies it to that source's
checkout. Reset discards staged and unstaged tracked edits and moves the
checked-out tip, as `git reset --hard` does. It does not add a general
`git clean` or reset other sources. The explicit reset intent is separate from
the existing flag that permits overwriting a pool ref. The same live
management authorization and revision ordering apply to reset, and the caller
can distinguish delivery from application. A retry of an already applied
reset must not wipe edits made after it succeeded.

### 3. Apply a complete revision through the sandbox agent

Use authenticated, revisioned delivery via the pool and sandbox agent, following
the convergence model in `pool-agent/sandboxruntime/runtimeconfig.go` and
`pool-agent/statuspoll.go`. The sandbox agent owns the install and reports the
revision it has installed. A sleeping box keeps its pending intent and
converges on the next start or thaw before a new harness session can read it.
Delivery is retryable and applies the latest desired revision without letting
an older retry roll it back. A box with an older agent reports that application
is unsupported until upgraded.

Managed skills take precedence over an
installed repository skill of the same name. The managed tree replaces the
whole visible skill while the revision supplies it. Removing the managed
skill restores the entry it shadowed, including an existing skills-source
link. Preserve originals and unrelated harness files, and never write through
a destination link into a checkout. A box with no managed skills continues
to use its existing installation.

Resolve the same run user's home and harness directories as the first-launch
installer. Stage and validate complete skill trees before publishing them.
Serialize publication with harness starts and acknowledge the revision only
after both skill directories reflect it. A failed apply leaves or restores
the previous complete revision for new sessions. A later invocation that
loads a skill from disk sees the replacement. A session that cached the old
instruction may need to reload it or start a new session. An optional local
event can carry the changed names and applied revision so a subscriber can
choose to reload. Delivery never types into the terminal.

### 4. Record the accepted change and its application

Write a box-scoped skill-change record in the same transaction as the desired
revision. Record the authenticated actor, operation, target box and revision,
and each affected name with its before and after provenance and digest or
commit. Record application separately on acknowledgement. The box's event log
exposes these records without content or credentials. A failed validation
creates no accepted-change record, and a delivery retry creates no second
accepted-change record. Keep the inventory available independently of event
replay so a subscriber can resynchronize after retention expires.

## Alternatives considered

- **Reuse the editable checkout.** This gives each box an upstream
  dependency and allows local edits to diverge from the manager's published
  version. It remains the right model for skills an agent is expected to edit
  and push back.
- **Watch and fetch refs in the control plane or pool.** This needs a
  service-side credential owner, revocation, scheduling and Git network
  policy. Revisit if managers need updates while no authorized publisher is
  running and a credential lifecycle for the watcher can be defined.
- **Give repository skills permanent precedence.** A pushed replacement
  would appear in the desired set but have no effect on an existing worker
  using the same name. A reversible override matches the specificity of
  create-time content skills and leaves the repository's declaration intact.
- **Copy files into home or send a prompt.** Neither operation establishes
  a desired revision, reports what the harness can discover, or accounts for
  stale helper files when a skill is replaced.

## Consequences

- The control plane owns desired skills and audit history. The box reports
  installed skills and applied revision. The manager can observe delivery
  without opening a terminal.
- Existing boxes need no rebuild solely to migrate their create-time data.
  They need a sandbox agent with support for managed skills to apply one.
- The exact push flags, endpoint shapes, digest encoding, storage and publication
  mechanics, event retention, and harness-specific reload support are open
  implementation details.
