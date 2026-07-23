# 0014 — `disco apply` pulls sandbox commits to the host via cherry-pick

- **Status**: Accepted
- **Date**: 2026-07-23

## Context

A sandbox's source starts as a copy of a host repository (ADR-0001: bind or
push, both git-based). Work then happens inside the sandbox — often by an
agent — and needs to land back in the host's working tree. Nothing today
closes that loop; the only transport ADR-0001 built (`worker-agent/githttp`
→ `sandbox_git_proxy.go`, addressed at
`/projects/{p}/sandboxes/{s}/git-repositories/{slug}.git`) is one-directional,
client → sandbox, used once at create.

This is not a novel problem. `codex apply` does the same job for OpenAI's
Codex cloud tasks: `codex apply <task-id>` fetches the task's latest diff
(a single flattened unified diff, byte-for-byte, from the ChatGPT backend —
not a commit, not a ref, not fetched from a repository at all) and runs
`git apply --3way` against whatever repository the CLI happens to be run
from. It does not verify the repository or base commit match the task, does
not create a commit (`--3way` implies `--index`, so a clean apply only
stages), and does not resolve conflicts itself: a three-way conflict leaves
ordinary `<<<<<<<`/`=======`/`>>>>>>>` markers and unmerged index entries,
`codex apply` reports the failure and exits, and nothing rolls back
(`codex-rs/chatgpt/src/apply_command.rs`, `codex-rs/git-utils/src/apply.rs`).
Confirmed against the currently-latest `rust-v0.145.0` release.

`disco apply` borrows the core move — land the sandbox's work as a patch
apply, never an automatic merge — but diverges from Codex's flattened-diff
model in one deliberate way: discobox sandboxes are real git repositories
reachable over discobox's own transport, not an opaque diff blob from a
cloud task, so `disco apply` fetches real commit history and can preserve
sandbox commit boundaries instead of squashing them into one diff (a
sandbox "could possibly be a bunch of commits," per the brief this ADR
implements). §3 below covers the mechanics and how conflict handling stays
close to Codex's "leave it, don't invent a resolution" stance while
adapting it to discobox's own usage pattern.

Three things are new territory, and are what this ADR decides:

1. **How to determine what to send.** A sandbox can accumulate any number of
   commits since it last diverged from the host, and the host branch may have
   moved on independently. There's no single "the" base commit recorded
   anywhere live — `GitSourceCheckout.Commit` is the commit the sandbox
   started from, fixed at create, not the current common ancestor.
2. **How to land it.** Merge and rebase both touch host branch state beyond
   just adding the sandbox's work — a merge commit records a permanent link
   between the two histories, a rebase can rewrite either side. Neither is
   what "apply" means in the prior art above.
3. **How to remember what was already applied**, across possibly many apply
   calls over a sandbox's lifetime, for possibly many sources on one sandbox,
   so a repeat call is a no-op and `disco ls`/API consumers can see sync
   status.
4. **How to keep that record trustworthy.** A multi-commit cherry-pick can
   fail partway through, same as a rebase — commit 3 of 5 lands, commit 4
   conflicts, and the operation stops mid-sequence. If `disco apply` recorded
   optimistically it would either record a partial, possibly-wrong result, or
   have to record nothing and lose track of the 3 commits that *did* land.
   Neither is acceptable: the record in point 3 needs to be true, or it's
   worse than no record at all.

## Decision

### 1. Transport: fetch over the existing git-repositories proxy

`disco apply` fetches each source's sandbox repository into the host repo,
using the same URL construction and bearer-token auth as
`cli/internal/sandboxcreate/deliver.go`'s push path, generalized for read
(`git-repositories` already grants fetch under `ScopeSandboxRead`, no new
server capability required). The fetched tip lands under a
discobox-owned ref, `refs/discobox/apply/<sandbox-id>/<slug>`, mirroring the
existing `refs/discobox/run/<id>` convention — visible to the user's normal
git tooling, not squirreled away outside the ref namespace.

No new server-side plumbing is needed for this step.

### 2. Base: computed via local `git merge-base`, not stored

After fetching, `disco apply` runs `git merge-base <fetched-ref> HEAD`
against the host repo to find the true common ancestor, rather than trusting
`GitSourceCheckout.Commit` (which may predate commits the host has made since
create) or requiring the server to track one. This is a plain local git
computation — no ADR-level state, always freely re-derivable from history
both sides already share.

If a source has a prior recorded apply (see §4), the base narrows to the
last-applied commit instead of the full merge-base, so a repeat apply only
sends what's new.

### 3. Landing: `git cherry-pick` attempted in a scratch worktree, fast-forwarded into the host only on full success

Git has no dry-run for "will this range of commits cherry-pick cleanly" —
the only way to find out is to actually attempt the three-way merges. So
`disco apply` attempts it for real, but never against the host's actual
checked-out branch directly:

1. `git worktree add --detach <scratch> <host-branch-tip>` — a disposable
   linked worktree off the host repo, sharing its object store, checked out
   at the exact commit the host branch is on right now.
2. `git -C <scratch> cherry-pick <base>..<fetched-ref>` — the real cherry-pick,
   run entirely inside the scratch worktree. The host's actual working tree
   and index are never touched by this step.
3. **On a clean result**, the scratch worktree's `HEAD` is now the exact,
   final resulting commit chain — known, real objects, not a prediction.
   `disco apply` fast-forwards the host's real branch onto it
   (`git merge --ff-only <scratch HEAD>`, run in the host repo). This is
   always a pure fast-forward — the scratch worktree started from the host
   branch's own tip and only added commits on top — so it either succeeds
   outright or fails for a reason worth surfacing on its own (see below),
   never partially. The scratch worktree is then removed. Only now does
   `disco apply` report success and record (§4) — at this point the outcome
   is no longer speculative, it already happened.
4. **On a conflict at any commit**, `disco apply` aborts the cherry-pick
   (`git -C <scratch> cherry-pick --abort`) and removes the scratch worktree.
   The host repository — branch, working tree, index — is left exactly as it
   was before `disco apply` ran; nothing was ever attempted against it.
   `disco apply` reports which sandbox commit conflicted and prints the exact
   commands to reproduce and resolve it directly, against the host's real
   working tree, run manually or by an agent:
   ```
   git fetch <repo-url> <fetched-ref>
   git cherry-pick <base>..FETCH_HEAD
   ```
   **Nothing is recorded to `AppliedCommits`** — the host state never
   changed, so there is nothing true yet to record. If the user (or an agent)
   then runs those commands and resolves the conflict themselves, that's a
   plain manual `git cherry-pick`, outside `disco apply` entirely; a later
   `disco apply` run will see the host is already caught up (§2's merge-base
   moves) and have nothing left to do.

The two failure modes the fast-forward step itself can hit are both
"nothing changed, try again" rather than partial state: the host branch
moved between step 1 and step 3 (someone committed meanwhile) fails the
fast-forward cleanly and `disco apply` retries the whole flow against the
new tip; the host's working tree has uncommitted local changes overlapping a
path the fast-forward would touch, and `git merge --ff-only` refuses to
clobber them, exactly as it would for a manual `git merge`. Both leave the
host repo untouched, same as a cherry-pick conflict.

Individual sandbox commits are preserved as individual host commits —
deliberate, since a sandbox "could possibly be a bunch of commits," and
squashing would throw away information the user didn't ask to lose. Each
commit's diff is replayed and reparented onto the host branch tip in order;
the sandbox's original commits and their place in the sandbox's history are
never referenced by the resulting host commits — only their content crosses
over. See "Consequences" for what that means for signatures.

Before attempting anything, `disco apply` checks each source's *current*
live working-tree state inside the sandbox via `git status --porcelain`
over the exec API (`CreateSandboxExecRequest`) — the fetch in §1 only sees
committed history, and an in-progress agent may have uncommitted changes that
fetch cannot see and that `apply` would then silently miss. An uncommitted,
dirty sandbox source is reported and skipped by default (with a flag to
proceed anyway later, once there's a concrete need); `disco apply`'s job is
to move committed work, not to guess at a commit message for someone else's
in-progress edit.

**Deferred**: launch a fresh sandbox scoped to resolving one conflicting
apply — seeded with the host's tip and the sandbox's unlanded commits, so an
agent resolves the conflict in an isolated environment purpose-built for it,
rather than in the user's own host checkout. Genuinely useful, but a second
piece of work built on top of a working `disco apply`; out of scope here.

### 4. Recorded state: a new `Sandbox.AppliedCommits` list, client-reported

```go
// AppliedSourceCommit records one successful disco apply of a source's
// commit into a host working tree. Client-declared provenance, like Origin:
// the server cannot observe host-side git state, so this is reported after
// the fact rather than verified.
type AppliedSourceCommit struct {
    Slug        string    `json:"slug"`               // which GitSource (primary or SourceCodeReferences key)
    Commit      string    `json:"commit"`             // original sandbox-side commit SHA that was cherry-picked
    HostCommit  string    `json:"hostCommit"`          // resulting host-side commit SHA (new object; see Consequences)
    HostID      string    `json:"hostId"`              // Origin.HostID of the applying client
    HostPath    string    `json:"hostPath"`            // absolute path applied into, on that host
    AppliedAt   time.Time `json:"appliedAt"`
}
```

`Sandbox.AppliedCommits []AppliedSourceCommit`, JSON-serialized like
`SourceCodeReferences`, **appended to, never replaced** — a sandbox may be
applied multiple times over its life, from possibly different host
directories or hosts, and the list is the audit trail. A new endpoint,
`CompleteSandboxApply` (shape mirrors the existing
`CompleteSandboxSourcePush`), is called once per source, only after §3's
fast-forward has actually landed the commits — never speculatively, and
never for a range that only partially applied, since §3's scratch-worktree
attempt guarantees the fast-forward is all-or-nothing. By the time
`disco apply` calls it, `Commit`/`HostCommit` are read off real objects that
already exist in the host repo, not predicted ones. `disco apply` reads the
list back before fetching to compute the narrowed base in §2 and to report
per-source sync status (e.g. a future `disco ls`/`disco box get` column).

This is a real field, not a git ref, because "was this sandbox's work synced
out" is exactly the kind of fact `disco ls` and other server-side consumers
should be able to answer without shelling out to git — unlike the
fetch ref in §1, which is disposable transport state.

### 5. Scope: all sources, matched by host

`disco apply` operates over every `GitSource` on the sandbox — the primary
`Source` plus every entry in `SourceCodeReferences` — not just the primary,
since a sandbox's secondary sources are just as capable of accumulating
sandbox-side commits. Each is addressed by its own slug on the
git-repositories endpoint and gets its own `AppliedSourceCommit` entries.

A source is only auto-resolvable to a host directory when
`Sandbox.Origin.HostID` matches the current client's own host ID (from
`cli/internal/origin`, per ADR-0001) **and** the source records a
`LocalDirectory`. Sources cloned from a remote URL, or created on a different
host, have no local directory to apply into automatically; `disco apply`
requires an explicit `--dir <slug>=<path>` for those instead of guessing.

## Alternatives rejected

**Merge or rebase onto the host branch.** Rewrites or advances host branch
state the user didn't ask to change, and risks history rewrites mid-work.
Patch-apply only ever adds commits on top of whatever's checked out — the
same "apply, don't merge" shape `codex apply` uses for the equivalent job.

**A single flattened diff via `git apply --3way`, matching `codex apply`
exactly.** Rejected on purpose, not by default: Codex's diff comes from an
opaque cloud task turn with no local git history behind it, so a single
diff is the only shape available. A discobox sandbox is a real, fetchable
git repository, so `disco apply` has actual commit boundaries to work with
and preserves them via `cherry-pick` instead of discarding them — directly
per the brief this ADR implements ("it could possibly be a bunch of
commits"). The tradeoff is real: Codex's model requires nothing from the
target repo's history (it doesn't even verify the base commit matches), while
cherry-pick's three-way merge requires the merge-base's blobs to exist
locally. Accepted, since a sandbox source and its host origin necessarily
share history through §2's merge-base computation.

**`git format-patch` + `git am --3way` instead of `git cherry-pick`.** Both
apply a range of commits individually and support the same resumable
conflict state; the difference is only in how they get there. `format-patch`
serializes local commits to text and `am` re-parses that text back into
commits — a round-trip with no purpose once the commits are already local
objects (§1's fetch), and one with real downsides `cherry-pick` doesn't have:
weaker handling of binary content, file mode changes, and renames, since it
operates on serialized text rather than tree objects directly. Kept as the
initial draft of this ADR before the redundancy was pointed out; superseded
in-place rather than re-proposed, since nothing had shipped against it.

**Cherry-pick directly onto the host's real checked-out branch, leaving
`CHERRY_PICK_HEAD` conflict state in the host working tree for an agent to
resolve on failure.** This was this ADR's own initial decision, superseded
in-place rather than re-proposed (nothing had shipped against it). It fails
point 4 in Context: a multi-commit range can conflict after several commits
already landed, at which point the host repo is genuinely, permanently in a
partially-applied state — and there's no way to tell `AppliedCommits` whether
those landed commits should be recorded (the operation isn't done) or not
(they did land, and a naive retry would try to reapply them). Isolating the
attempt in a scratch worktree first removes the ambiguity entirely: failure
there can never leave partial state in the host, so recording is always
either "all of it, provably" or "none of it." The cost is that a human
running `disco apply` interactively no longer gets dropped into a live
conflict to resolve in place — they get a command to run themselves instead,
which is a deliberate trade documented as a consequence below.

**A true dry-run via `git merge-tree` (or `--no-commit` staging tricks)
instead of a scratch worktree.** `git merge-tree --write-tree` can compute
whether *one* three-way merge would conflict without touching the working
tree or index at all, which sounds like a better fit for "check before
committing." It doesn't fit here: `disco apply` needs to sequence a chain of
individual cherry-picks, each with its own author/committer/message,
replayed one at a time — `merge-tree` computes a single tree-level merge
result, not a commit sequence, so using it would mean reimplementing
cherry-pick's own sequencing and per-commit metadata handling on top of
lower-level plumbing. A scratch worktree gets the identical outcome (full
isolation, nothing touches the host until success is certain) by reusing
`git cherry-pick` exactly as-is, including its existing conflict reporting.

**Exec-based git status/log for the whole inspection step**, instead of
fetch+local-diff for history. Parsing `git log`/`git status` output over the
exec API for every commit would duplicate what `git merge-base`/
`git cherry-pick` already do correctly and cheaply once the commits are
fetched locally. Exec is used surgically, only for the one thing fetch
cannot see: current uncommitted dirtiness.

**Store the applied-commit marker as a client-side git ref**
(`refs/discobox/applied/<sandbox-id>/<slug>`), mirroring `refs/discobox/run/
<id>`, instead of a server field. Cheaper (no schema/API change) and
consistent with how other transfer state lives in refs, but invisible to
`disco ls`/the API/other clients — and "was this sandbox's work already
pulled out" is a question worth answering without a local git checkout in
hand, e.g. to warn against deleting a sandbox with unapplied work.

**A single scalar `LastAppliedCommit`/`LastAppliedAt` per sandbox**
(mirroring `SourceDeliveredAt`). Doesn't fit once sources are plural (§5) or
once a sandbox can be applied more than once — both true here — and would
need to become a list eventually anyway.

**A dedicated `SandboxSourceSync` resource/table.** More extensible (its own
lifecycle, its own store package) but pure overhead for what is, today, an
append-only client-reported log with no independent lifecycle of its own. A
list field is the smaller structure that fits the actual shape of the data;
promote to a resource if it needs one later (e.g. per-entry deletion,
pagination at scale).

## Consequences

- **Signed commits are handled transparently, with one caveat worth stating
  plainly.** `git cherry-pick` reparents each sandbox commit onto a different
  parent, which produces a new commit object with a new SHA regardless of
  signing — so any signature the sandbox put on its original commit cannot
  and does not carry over; that's inherent to replaying a commit onto new
  history, not a discobox choice. The new host commit is then signed exactly
  like any other commit the user makes locally: `disco apply` shells out to
  real `git` in the host repo, so the host's own `commit.gpgsign`/
  `user.signingkey`/SSH-signing config and `-S` behavior apply unmodified —
  no discobox-specific signing logic is needed. `AppliedSourceCommit` records
  both SHAs (`Commit` for the original sandbox-side commit, `HostCommit` for
  the new signed-or-not host object) so "was this signed by me or by the
  sandbox" is always answerable from the audit trail rather than assumed.
- New API surface: `CompleteSandboxApply` (or equivalent), `AppliedCommits`
  on the `Sandbox` model/OpenAPI schema, a store column
  (`applied_commits`, JSON-serialized list — no migration required, per
  the project's disposable-DB stance).
- `disco apply` needs a small new exec-git helper (dirty-check only, per
  §3) — the first place discobox parses git output over the exec API.
- On conflict, `disco apply` hands back a command rather than a live conflict
  state to resolve — the opposite of this ADR's earlier draft. A human
  running interactively has to copy-paste and run it themselves (or an agent
  does); no in-progress cherry-pick is ever left inside their existing
  checkout to just continue. Correctness (§4's atomic record) won out over
  that convenience; the deferred "spin up a sandbox to resolve it" idea in
  §3 is the intended way to make that step feel less manual later.
- Every apply attempt costs one `git worktree add`/`remove` pair. Cheap — a
  linked worktree shares the host repo's object store, it's not a clone —
  but it's real filesystem churn per apply, worth confirming stays cheap on
  very large repos if that ever comes up.
- A sandbox applied from a host that doesn't match `Origin.HostID`, or for a
  remote-cloned source, always needs an explicit `--dir`; there is no
  auto-discovery across machines. Consistent with ADR-0001's stance that
  `LocalDirectory`/host identity is meaningful only on the host that owns it.
- `AppliedCommits` grows unboundedly over a long-lived sandbox's life with
  repeated applies. Not a practical concern at expected scale (a handful of
  applies per sandbox); revisit only if that assumption breaks.

## Work order

1. `AppliedSourceCommit` + `Sandbox.AppliedCommits`; `CompleteSandboxApply`
   endpoint and OpenAPI schema.
2. Fetch helper generalizing `deliver.go`'s URL/auth construction for read,
   landing at `refs/discobox/apply/<sandbox-id>/<slug>`.
3. Exec-based dirty-check helper (first exec-git-parsing code in the repo).
4. `disco apply` CLI command: sandbox/source resolution (including
   `--dir` for unmatched hosts), merge-base + scratch-worktree cherry-pick
   + fast-forward (§3), `CompleteSandboxApply` reporting only on success,
   conflict-command messaging on failure.
5. `disco ls`/`disco box get` surfacing of per-source apply status, once the
   field exists to show.

On completion, update `cli/DESIGN.md` (new command) and `server/DESIGN.md`
(new endpoint, extended `Sandbox` shape) to describe the resulting system.
This ADR is not edited afterward.
