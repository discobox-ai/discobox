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
  This inference belongs to the prompt path only — `disco box sandbox create` is
  flag-driven and reads nothing from the environment.
- Whether a dirty workspace is snapshotted is the caller's policy
  (`PromptOptions.IncludeDirty`), and asking is the caller's UI
  (`ConfirmIncludeDirty`); this package decides only when the question applies.
  See the CLI design doc's "Uncommitted Work at Create".
- `PromptOptions.Include` names extra sources, resolved exactly as the primary
  source is and filed as the request's `sourceCodeReferences` under the sandbox
  directory each lands in. This package settles their slugs and destinations;
  see the CLI design doc's "Extra Sources".
- A source directory in no Git repository gets a throwaway repository built over
  it, and is delivered by push. Create returns every source's repository as
  `LocalSources`, which delivery pushes out of and the caller closes; see the
  CLI design doc's "A Directory That Is Not a Repository" and
  [ADR 0045](../../../docs/adr/0045-a-directory-with-no-repository-is-delivered-by-push.md).
- Do not depend on `internal/cli` or `internal/tui`. Both frontends consume this
  package through their adapters.
- Keep terminal waiting, attach, progress output, and rendering in the frontend
  packages; those behaviors begin after sandbox creation.
- The sandbox name is generated here (`randomname`), and sandbox names are
  unique within a project, so `CreatePromptSandbox` retries a 409 with a fresh
  name a bounded number of times. Only a generated name is replaced this way: a
  name the user typed is theirs, so `disco box sandbox create --name` reports
  the conflict instead of quietly creating something else.
