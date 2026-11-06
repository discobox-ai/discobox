# 26-11-05-928 — A directory snapshot survives until source delivery is confirmed

- **Status**: Accepted (supersedes 0045 §4's throwaway lifetime and
  narrows 0045 §1's temporary storage requirement and 26-09-24-005 §2's
  refusal for a source with no repository; origin-host restrictions remain)
- **Date**: 2026-11-05
- Accepted by the owner by merging #710, 2026-11-06
- **Issue**: [#685](https://github.com/discobox-ai/discobox/issues/685)

## Context

A non-Git directory is represented by an empty base commit and a workspace
snapshot, in a repository outside that directory. `LocalDirectory` remains
its original path. This preserves the files as uncommitted work in the box,
without creating `.git` in the person's directory (0045 §§1–2, 0073, 0077).

At `b852aae3`, `cli/internal/sandboxcreate/source.go` owns that repository
through `LocalSources.Close`. `cli/internal/cli/run.go` closes it even after
failed delivery. `cli/internal/cli/sourcepull.go` prints an attach retry, but
`deliverParkedSource` in `cli/internal/cli/push.go` resolves the original path
with `gitutil.Root`, and `sandboxcreate.CheckDeliverable` rejects every
`noLocalRepository` source. The console uses the same delivery machinery.
The snapshot cannot be reconstructed from files that may have changed.

The reported HTTP 502 does not identify the failing downstream route. The
public `git-origins/{slug}.git` URL serves both legacy sandbox origins and
pool-cache origins. `server/DESIGN.md` and
`cli/internal/sandboxcreate/cachedeliver.go` establish that cache delivery
starts before sandbox provisioning finishes. Waiting for every sandbox to
park would reverse 26-10-10-545 §3 without explaining this failure.

## Decision

### 1. Pending snapshots are private durable client data

Before submitting a create, `sandboxcreate` saves each non-Git source as a
self-contained repository under
`<os.UserConfigDir()>/discobox/client/pending-sources/`. This is recovery state,
not the remote Git cache under `os.UserCacheDir()`: cache eviction must not
remove the only copy of a pending source. The original directory remains
untouched and remains the source's API provenance, grouping and apply target.
No local recovery path is sent to the server.

The saved repository contains the empty base, the captured snapshot when
present, and refs retaining both objects. It must not depend on an alternate
object store, on the original working directory, or on temporary files. Retry
uses only saved objects; it never indexes the original directory again.
The manifest records the exact snapshot object ID as well as its ref name,
including an explicit absence for an empty snapshot.

Directories are private to the current user (0700 on POSIX), manifests and
repository data are not readable by other users, and Windows uses equivalent
user-only access. Publish a complete repository and versioned manifest
atomically before issuing create; a failed write aborts before create. Never
save credentials, bearer tokens or credential-bearing endpoint URLs in it.

### 2. Recovery identifies the original source, not today's directory

Each entry has a random local identifier and records the selected canonical
server endpoint without credentials, project ID, origin host ID, source key
and slug, original directory, checkout commit/ref, workspace mode/base/ref and
captured snapshot object ID. After create returns, atomically bind the entry
to the returned sandbox ID and the server's recorded source identity.
Endpoints are deliberately scoped as selected by the client; another alias
is not silently treated as the same server.

An interrupted create can have reached the server before its response or the
binding write reached disk. Preserve the staged entry. Recovery may bind an
unbound entry only when its endpoint, project, host and complete recorded
source tuple match the sandbox; require exactly one matching entry. No lookup
by directory alone, newest entry, branch tip or current file contents is allowed.
A mismatch or ambiguous match fails before delivery or repair.

One resolver owned by `sandboxcreate` supplies saved repositories to initial
run/console delivery and later attach, explicit push and console start/open
recovery. Callers pass endpoint/project/sandbox identity; they do not each
implement a filesystem search. For `noLocalRepository`, use this lookup before
attempting `gitutil.Root`. Validate the manifest, pinned commit and snapshot
object/ref correspondence before any source is sent or a failed box repaired.
Preflight all sources, including extra non-Git sources, as today.

Attach retains its origin-host and sandbox-origin restrictions
(26-09-24-005 §2 as subsequently narrowed by 26-10-10-545 §1, and the current
`deliverableHere` guard). Saved data is not authority to deliver another
machine's or a creating sandbox's source. `--dir` does not cause a fresh
snapshot or bypass these checks. Ordinary pushes after initial delivery keep
their existing repository and lease semantics.

### 3. Confirmation, not process exit, ends retention

`LocalSources.Close` releases in-process resources; it no longer deletes an
entry that might belong to an accepted create. Cancellation, HTTP failure,
timeout, an ambiguous create response, a source-await failure and console exit
all retain the saved snapshot. Partial multi-source delivery retains the whole
pending set until the sandbox's aggregate delivery is confirmed.

Remove bound entries only after a successful completion acknowledgment or an
authenticated reread of this sandbox confirms `sourceDeliveredAt`. A lost
completion response therefore retains the data until the next successful
read. Concurrent delivery and cleanup take a per-entry process-safe lock;
cleanup cannot delete objects another delivery is reading. Existing idempotent
delivery and within-process joining rules remain (26-09-24-005 §3).

A create definitively rejected before acceptance may remove its entries.
There is no age-based or size-based eviction of pending or ambiguously accepted
entries. An unreachable server, denied read, missing listing row or archived
box is not proof that recovery data is disposable. Abandoned entries remain
until the person explicitly removes them. On failure, show the retained path,
the exact existing attach retry command, and explain that deleting that path
forfeits recovery. This also provides a manual discard path for entries whose
create outcome cannot be resolved; no new management command is required.
Automatic pruning after authoritative purge confirmation is deferred until a
reliable deletion signal and a client maintenance surface exist.

### 4. A missing snapshot is a refusal with an accurate way forward

Previously deleted snapshots cannot be backfilled. Missing, corrupt or
mismatched data fails with the source slug and pinned identity, explaining that
the original snapshot is unavailable on this client. Say to retry on the
original client if it still holds that snapshot, otherwise create a new box
from the current directory. Never advertise `push --dir`, `git init`, or a new
snapshot as recovery of the existing pinned files. No files changed since
create are silently substituted.

### 5. Keep delivery readiness specific to its destination

Cache delivery continues immediately after create, waiting for the pool as it
does today. Legacy push delivery continues to wait for `awaiting_source`.
Both retain their snapshots on failure. Fixing the cold-provisioning 502
requires tracing its public request to the chosen downstream route and readiness
condition; this ADR does not assert an unobserved cause or authorize a blanket
sandbox readiness wait. The route-specific cause and correction remain open
to implementation, within these delivery contracts.

## Alternatives rejected

- **Keep the temporary directory only on a reported error.** A process crash
  or lost response can precede that branch; temporary-directory cleaners can
  remove it before attach. Publish durable recovery data before create.
- **Re-snapshot the original directory on retry.** It may have changed or
  disappeared. A new snapshot is a different source from the one accepted.
- **Initialize Git in the person's directory.** Changes their files and still
  cannot reproduce an already lost pinned snapshot (0045).
- **Use the remote Git cache or an automatic expiry.** Either makes a cache
  cleaner or elapsed time destroy the only recovery copy. The cost of retaining
  abandoned snapshots is explicit instead of hidden behind a working retry hint.
- **Upload the snapshot to the server as part of create.** Adds another durable
  source transport and server-side source storage before the existing pool Git
  delivery. Local retention repairs retry without changing that boundary.
- **Store only sandbox-ID bindings.** Loses discoverability if create succeeds
  but its response or the binding write is interrupted. The staged exact-match
  lookup closes that gap without adding a public API field.

## Consequences and acceptance evidence

Failed non-Git creates retain a private copy that can consume disk indefinitely
until delivery succeeds or the person discards it. Another machine cannot
recover that local copy automatically. Persistent storage of copied files is
new behavior; failure output must disclose it. Successful delivery removes the
local copy without altering the files in the original directory.

Implementation must exercise a real non-Git directory with initial delivery
forced to fail, exit the creator, mutate or remove the directory, then execute
the exact printed Cobra attach command in a fresh process against a working
origin. Verify the original pinned bytes arrive, completion starts the box and
retained data is removed only after confirmation. Exercise console create/exit
and reopen through the same resolver. Cover extra sources, empty directories,
interrupted binding, lost completion response, concurrent retry/cleanup and
missing or mismatched state. Trace the cold-start failing route separately and
verify its actual readiness condition, including that cache delivery can finish
before the sandbox boots. No runtime implementation is part of this proposal.
