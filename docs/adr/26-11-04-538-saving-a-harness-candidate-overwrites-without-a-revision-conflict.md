# 26-11-04-538 — Saving a harness candidate overwrites without a revision conflict

- **Status**: Proposed (would supersede [26-10-30-775](26-10-30-775-a-user-can-force-save-a-reviewed-harness-candidate.md)'s ordinary-save conflict and explicit-force policy, and narrow [26-10-26-573](26-10-26-573-a-harness-exports-local-configuration-back-to-its-config.md) §3's revision conflicts and mandatory reconsideration for save-back)
- **Date**: 2026-11-04
- **Issue**: [#566](https://github.com/discobox-ai/discobox/issues/566)

## Context

The owner wants saving a harness candidate to overwrite the shared configuration
without a conflict check or a second overwrite confirmation. The reviewed diff
is sufficient to choose to save. Configure is being removed in separate work;
changing that retiring flow's concurrency policy would not settle how the
remaining save-back works.

Today there are two different checks:

- `CommitHarnessConfigConfigure` in
  `server/internal/resources/harnessconfigs/configure.go` compares the config
  and bound-secret revisions recorded when configure started with those at
  commit. It reports `harness configuration changed; reconsider the candidate
  before saving` on either mismatch, then deletes the setup sandbox. That flow
  has no candidate to reconsider.
- `commitCapture` in `server/internal/resources/harnessconfigs/capture.go`
  compares the candidate's write revision and secret revisions with the current
  rows. ADR 26-10-30-775 permits bypassing those comparisons with `--force`.
  The console offers a second confirmation after a revision conflict.

Investigation of the reported configure error found that two consecutive
configure runs with unchanged state succeed. Advancing only a bound secret via
`UpdateSecretValueIfUnchanged` between start and commit reproduces the exact
reported error, while the config revision still matches. This is the store
operation OAuth renewal uses. Claude's configure script verifies a credential;
verification through a reused sentinel can cause renewal. The owner's failing
server was unavailable to this worker, so renewal is a demonstrated cause of
this error, not a confirmed explanation of why every reported run failed.
There is no evidence yet of a fileOverrides or manifest-digest defect.

The owner subsequently clarified that this issue should establish overwrite as
the normal save behavior, given the separate removal of configure. That policy
reverses the accepted ordinary-save rule regardless of which writer triggered
the historical configure failures.

## Decision

### 1. Saving applies the candidate even when revisions have advanced

An authenticated user's save of an immutable harness candidate authorizes its
file and credential changes to win over current shared state. Neither a config
revision change nor a bound-secret value revision change rejects that save.
There is no automatic reconsideration, revision-conflict response, or second
confirmation. A change after the preview and before commit does not invalidate
the user's save; transaction order decides which write wins at a shared path
or credential.

The candidate remains a patch. Its listed file writes and removals apply to the
current config, using ADR 26-11-04-028 §5's override/configured-file ownership.
Unlisted paths and unrelated config fields remain as they stand at commit.
Omitted credentials remain unchanged. A credential marked `keep` keeps the
current binding and value; it never replays the sandbox's old credential.
A captured replacement replaces the config's credential even if the server
renewed it since capture. A hand-bound secret remains someone else's: the save
creates a config-owned replacement and changes the harness binding instead of
writing that secret in place, as the current force-save does.

A preview still shows the candidate against current stored settings and
redacted credential metadata. It is a view, not a promise that the remote
state cannot change afterward. Reconsider may remain available for a person
who wants to select a different patch; saving never requires it.

### 2. Ordinary save is the only overwrite mode

The server's save-back API uses this behavior with no force parameter. Remove
`admin harness save --force`, the revision-conflict problem type and the
console's conflict/retry confirmation path together. Clients submit the chosen
candidate once. The API's canonical OpenAPI definition and generated clients
change with the server and CLI.

This changes save-back only. Direct harness editing and other resource writers
retain their existing revision contracts. Configure's removal belongs to the
separate work; this decision requires no change to its temporary commit flow.

### 3. Commit integrity and candidate validation still apply

The save transaction locks and reads current config and bound-secret rows,
then applies the patch with the store's compare-and-swap against those current
rows. It persists files, credentials, bindings, dirty marks and the idempotency
receipt atomically. Revision counters still identify changes and delivered
provenance; they no longer make a captured candidate ineligible to save.

Authentication, project scope, the sandbox's current harness assignment,
manifest compatibility, immutable candidate identity, registered provider and
file validation, sentinel rejection, credential sealing and acknowledged
handoff retain their existing checks. A missing binding needed by `keep`, an
invalid credential reference, or a changed harness definition still fails with
its actual recovery reason. These failures do not offer a force retry.

Receipt replay returns the accepted result and retries pending handoff rather
than applying the candidate again. Delivery baselines continue to describe
what the sandbox received; an overwrite does not rewrite that history.

## Alternatives rejected

- **Fix configure's check and preserve ordinary save's conflicts.** Configure
  is being removed, and this would leave the owner's requested save policy
  dependent on a force flag and a second console confirmation.
- **Compare only fields shown in the diff.** Avoids conflicts from unrelated
  changes but still rejects the deliberate save when one of those fields
  changed. The owner wants the chosen candidate to win in that case too.
- **Keep `--force` as the normal workaround.** Makes callers remember a second
  mode for the intended behavior and keeps the console's extra prompt.
- **Refresh revisions and retry after a 409.** Retains a failing first request
  and a race before retry. Applying the patch to locked current rows expresses
  the policy directly.
- **Replace the entire config snapshot.** Would overwrite unrelated paths and
  settings the candidate never asked to change. Overwrite applies to the
  candidate's patch, with the existing ownership rules.

## Consequences

Two people saving different values at the same path get the last committed
value. The same applies to captured credentials: a saved local OAuth value may
already be stale after an upstream refresh. This is an explicit consequence of
choosing save as overwrite, not a problem revision checks can solve while also
allowing every such save. A fresh login may be needed after a stale replacement.
No additional confirmation is introduced for that case.

Implementation must demonstrate repeated saves immediately after viewing the
diff, and saves after a real concurrent config edit and credential renewal.
Those cases must succeed, the candidate's changed fields must win, unrelated
fields must survive, and receipt replay must not reapply credentials. Invalid
candidates, changed harness definitions and unauthorized saves must still fail.
The console must save once without an overwrite dialog. The implementation
updates the affected current-state design docs and removes superseded conflict
code across the API, server and clients. This Proposed ADR changes no runtime
behavior and leaves #566's implementation acceptance criteria outstanding.
