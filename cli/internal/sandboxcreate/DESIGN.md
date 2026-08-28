# Sandbox Creation

This package owns UI-independent client-side preparation and submission of
sandbox create requests.

- Frontends provide typed options; this package resolves source refs, snapshots
  dirty local workspaces, captures local user identity, captures the local Git
  authorship, classifies environment and secret inputs, builds the API body, and
  submits prompt sandbox creates.
- Git authorship is read with git's own resolution from the source directory, so
  a repository-local `user.email` beats the global one. Unset stays unset: git is
  the authority on whether an identity is configured, and a `$USER@$(hostname)`
  fallback here would only relocate the wrong answer
  ([ADR 0042](../../../docs/adr/0042-git-authorship-identity-is-a-first-class-sandbox-property.md)).
  This inference belongs to the prompt path only — `discobox admin discobox create` is
  flag-driven and reads nothing from the environment.
- Local user identity is captured from the client machine, except on Windows,
  which has no POSIX identity to capture and instead sends a fixed one: `discobox`,
  uid/gid 1000, home left to the sandbox. Translating a SID and a `C:\Users`
  home yields nothing usable, and sending nothing means the image's own user
  ([ADR 0025](../../../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
  §5) — root on most harness images. A stated identity is not a guess about the
  local machine, because there is no local answer to get wrong.
- A local source keeps its own absolute path inside the sandbox, so a path means
  the same thing on both sides of the boundary. On Windows it keeps that path in
  the spelling WSL gives it: `E:\src\project` becomes `/mnt/e/src/project`, the
  drive letter lowercased because that is how `/mnt` is spelled and the rest left
  in the case it has on the host. A Windows path cannot be mirrored verbatim —
  the sandbox runs Linux, and the daemon rejects `E:\src\project` as not
  absolute — but it already has a POSIX name, so the mapping stays one-to-one
  instead of collapsing every source onto one container directory. A path with no
  drive letter has no `/mnt` name — a UNC share, or a path already inside a
  distro — and falls back to `/workspace/source`, with the requested
  subdirectory honored by its position within the repository.
- A source's path on this machine and its path in the sandbox are therefore two
  different things off a POSIX host, and the code that reads the client's disk —
  `.discobox/sources.json`, the checkout beside the primary source — takes the
  first, while destinations and reference keys take the second.

- Whether a dirty workspace is snapshotted is the caller's policy
  (`PromptOptions.IncludeDirty`), and asking is the caller's UI
  (`ConfirmIncludeDirty`); this package decides only when the question applies.
  See the CLI design doc's "Uncommitted Work at Create".
- A source directory in no repository asks the same question about the whole
  directory (`ConfirmCopyDirectory`), answered by the same `IncludeDirty`
  policy, and this package measures what would be copied for the frontend to
  show while it asks (`MeasureDirectory`, polled through `DirectoryWalk.Total`).
  Both frontends count the same thing the same way; only where the number is
  drawn is theirs.
- Resolution can answer "no source": declining that question returns the zero
  `resolvedRunSource` (`resolved()` reports it), and the request is built with no
  `config.source` exactly as `NoSource` builds it. A reference that answers the
  same way is dropped instead. An empty directory is a different case — it is not
  asked about and keeps its source at its own path.
- `PromptOptions.NoSource` builds a request with no `config.source` at all —
  the shape the harness configure sandbox already had, reached deliberately.
  `Source` is then only what the origin and the Git authorship are read from, so
  a sourceless sandbox is still one you started here and listed here. A ref is
  refused rather than dropped, since there is nothing to check it out of; extra
  sources are unaffected, and declared ones fall away with the checkout that
  would have declared them. See the CLI design doc's "No Source At All".
- `PromptOptions.Include` names extra sources, resolved exactly as the primary
  source is and filed as the request's `sourceCodeReferences` under the sandbox
  directory each lands in. This package settles their slugs and destinations;
  see the CLI design doc's "Extra Sources".
- The primary source's own `.discobox/sources.json` names more of them
  (`declared.go`). It is read here, on the client, because deciding what a
  declared source resolves to means looking at this machine's disk for a
  checkout of it — which is also why it is not a field of `.discobox/project.json`,
  read by pool-agent out of the materialized clone. Reporting is the frontend's
  (`ReportDeclaredSource`); this package prints nothing. See the CLI design
  doc's "Declared Sources" and
  [ADR 0056](../../../docs/adr/0056-a-repository-declares-the-sources-it-is-worked-on-with.md).
- Delivery is also reachable from outside a create: `PendingSourcePushes`,
  `NewLocalSources`, and `CheckDeliverable` let `discobox push` hand a discobox
  parked in `awaiting_source` the source its create never delivered, out of the
  repositories still on the client. The push and the completion call are the
  same `DeliverSource`; only where the repositories come from differs — a create
  has just resolved them, a later delivery finds them again from each source's
  recorded local directory. See the CLI design doc's "Re-delivering Source".
- A source directory in no Git repository gets a throwaway repository built over
  it — after the user has been asked whether to copy it at all — and is
  delivered by push. Create returns every source's repository as
  `LocalSources`, which delivery pushes out of and the caller closes; see the
  CLI design doc's "A Directory That Is Not a Repository" and
  [ADR 0045](../../../docs/adr/0045-a-directory-with-no-repository-is-delivered-by-push.md).
- Do not depend on `internal/cli` or `internal/tui`. Both frontends consume this
  package through their adapters.
- Keep terminal waiting, attach, and rendering in the frontend packages; those
  behaviors begin after sandbox creation. The one thing that does not begin
  after it is naming the steps taken here: `CreatePromptSandbox` and
  `DeliverSource` take a `Report` and call it as they enter each one, because
  no round trip can tell a process what it is currently doing
  ([ADR 0060](../../../docs/adr/0060-provisioning-progress-is-a-recorded-phase-the-client-polls.md)).
  The words are `Step` constants here so the two frontends cannot describe one
  stage differently; where the line is drawn and when it is cleared is theirs.
- Not every line is a step this client takes. `ProvisionStatus` renders what the
  pool agent recorded on the discobox — a phase, and for a pull its byte and
  layer counts — and `awaitSourceRequested` reports it through the same `Report`
  as it waits, out of the reads that wait is making anyway. It lives here rather
  than in a frontend for the same reason the `Step` constants do, and because
  the other narrated wait — `internal/cli`'s attach watch — renders from it too.
  A discobox with nothing left to provision renders nothing, which leaves the
  caller's own step standing rather than blanking it.
- The sandbox name is generated here (`randomname`), and sandbox names are
  unique within a project, so `CreatePromptSandbox` retries a 409 with a fresh
  name a bounded number of times. Only a generated name is replaced this way: a
  name the user typed is theirs, so `discobox admin discobox create --name` reports
  the conflict instead of quietly creating something else.
