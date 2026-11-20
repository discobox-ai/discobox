# Harness Review Notes

- There is no setup step (ADR 26-11-02-152). A launcher must reach the CLI's
  sign-in with no credential delivered, and treat a missing credential file as
  signed out, never as an error: every box on a harness with nothing saved
  starts that way.
- Every launch-time import and handoff must consume the current credential
  document format. Changing export/installation without updating them can
  leave valid JSON objects where the vendor expects a token string and
  silently break a delivered login.
- A bundle declaring `harness.config` is refused at registration; do not
  reintroduce a setup command, configure ports, or a script that writes a
  harness config's result. A sign-in reaches the config only as a saved
  harness change.
- Hold `~/.config/discobox/harness-capture.lock` around every Discobox-owned
  read/modify/write of credential-bearing state, including startup initialization.
  Otherwise a stale initializer snapshot can restore a raw login after handoff
  has replaced it with an acknowledged sentinel, or lose concurrent trust edits.

- Pass host paths into POSIX test launchers as quoted data, not interpolated
  shell source. Windows backslashes are consumed as shell escapes, and spaces,
  quotes and dollar signs in a checkout path can change the command before the
  tested program sees it. The generator single-quotes every declared value it
  writes into a launcher, not only those `Validate` leaves free, and
  `internal/harnesstest` refuses a test path a single quote cannot hold;
  exercise a path with spaces and dollar signs locally.

- Prove Codex folder trust with an untrusted control against the pinned
  binary, without explicit approval/sandbox policy masking onboarding.
  A prompt reached with those policies alone does not prove folder trust.
  Check direct starts after the launcher and retain earlier system trust
  entries when a later launch runs in a different folder.

- A settings key a harness CLI writes by itself, before any input, is local
  bookkeeping and goes in its declaration's `capture.settings` exclude. Otherwise
  every fresh box opens the save-back prompt (Codex: `tui.model_availability_nux`,
  then `tui.screen_reader_detection_done`). CLIs install at their latest
  release, so a new such key arrives with no change here. When touching a
  harness's settings, start a fresh box, give it no input, and check that
  `admin harness changes` reports `clean`.
- A built-in changes through its `harness.json`, its `adapter.ts`, its
  assets or the shared runtime (ADR 26-11-06-596). Never hand-edit what
  `discobox-harness-bundle` stages, and never add a per-harness Dockerfile,
  Go package, shell script or watcher entry: if the shape cannot say it, the
  shape changes, with its validation and tests.
- `harness/runtime` never branches on a harness's ID. A vendor difference is
  an adapter operation the declaration selects (`operations` in
  `runtime.ts`), named for what it does, typed in `runtime/types.ts`, and
  tested in `runtime.test.ts`.
  An inline shell string in a declaration is not a strategy.
- A changed file under `internal/harnessbundle/testdata/labels` is a change
  to a published label, the contract every registered config snapshots.
  Review it as one: a renamed secret, provider, command or path strands
  saved configurations unless a migration carries them.
- An installed path, home path, capture command or credential document shape
  never moves in a refactor (ADR 26-11-06-596 §5); a move needs its own
  idempotent migration.
- Harness TypeScript is erasable syntax only, run as written: no `enum`,
  namespace or parameter property, `import type` for every type, and `.ts`
  on every relative import. The generator compiles a file without its
  neighbours and rewrites only relative `.ts` imports to `.mjs`, so anything
  that needs a type checker or a bundler to emit breaks on the box, not in
  `check:ts`.
- A vendor document is `Vendor` (any) only at the edge where it is parsed;
  what the runtime and an adapter exchange is typed in `runtime/types.ts`.
  Widen a contract type there, with its reason, rather than casting around it.
