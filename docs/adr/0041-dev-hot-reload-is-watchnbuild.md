# 0041 — Dev hot reload is watchnbuild, not Air

- **Status**: Proposed
- **Date**: 2026-08-14

## Context

`task dev` rebuilt and restarted discobox-server through
[Air](https://github.com/air-verse/air), configured by `.air.toml`, with a
second config `.air.cli.toml` behind `task dev:cli` that only relinked the CLI.

Three things about that setup were wrong in ways the config could not express:

- **The watch set was an allowlist that had gone stale.** `include_dir` named
  thirteen directories, seven of which no longer exist at the repo root, and
  omitted root packages that `server/` and `cli/` actually import — `internal`,
  `execstream`, `devimage`, `harness`, `localipc`, `layout`, `controlplane`,
  `randomname`. Editing any of them did not trigger a rebuild. Nothing failed
  loudly; the server just kept running stale code.
- **Air has no restart backoff.** ADR 0019 makes a starting server wait on
  `<data dir>/server.lock` rather than displace the incumbent, and a start can
  also lose the race for the listen socket. Both are transient and neither is
  resolved by editing a file, but Air's only restart trigger is a file change,
  so a rebuild that lost the race sat dead until the developer touched
  something.
- **Build-only is not a mode Air has.** Air supervises a long-lived process, so
  `.air.cli.toml` ran `sh -c 'echo ...; sleep 2147483647'` as a stand-in
  process for a loop that only ever needed to compile.

The two configs had also acquired an outright bug: both ran `rm -f build/disco`
before building, so a failed compile left the developer with no CLI at all
rather than the previous one.

## Decision

Drop Air. `task dev`, `task dev:server`, and `task dev:cli` run
[watchnbuild](https://github.com/ibuildthecloud/watchnbuild), configured by
`.wnb.yaml` and `.wnb.cli.yaml`. The `air-verse/air` tool directive is removed
from the root and `server` modules, and `.air.toml` / `.air.cli.toml` are
deleted.

The three points above map onto wnb features rather than onto config we
maintain:

- The server config watches the whole tree and excludes what a build writes,
  so a new root package is covered the day it is added. `.wnb.cli.yaml` keeps
  an allowlist deliberately — it is normally run alongside `task dev`, and
  server edits should not relink the CLI.
- `retry` gives a failed start exponential backoff from 15s to 10m, which is
  the shape of the ADR 0019 lock wait.
- Omitting `run` is a build-only loop, so the CLI config has no placeholder
  process.

Neither config removes its output before building: `go build` replaces a binary
atomically, so it works over a running process and leaves the previous binary
in place when compilation fails.

## Alternatives rejected

- **Fix `.air.toml` in place.** The stale `include_dir` was repairable, but the
  allowlist is the failure mode, not its contents: it silently stops covering
  each new root package, and the last person to notice was several packages
  late. Restart backoff and a first-class build-only mode are not config
  problems at all — Air has neither.
- **Keep both, and let developers choose.** Two supervisors mean two configs
  that drift, and the stale-allowlist bug is exactly what drift produces. The
  parallel `task dev-wnb` existed only to trial wnb, and ends with this
  decision.

## Consequences

- `build-errors.log` is gone. Air wrote build failures to a file; wnb writes
  them to the terminal it runs in, so the gitignore entry and the `clean`
  target's reference to it are removed.
- Anything that greps for a `go tool air` process to find the real dev server —
  `.agents/skills/verify` did — must look for `watchnbuild` instead.
- ADR 0019 still describes its restart-window race in Air's terms. It is
  history and stays as written; the lock behaviour it decided is unchanged, and
  wnb's `retry` block is where that wait is now absorbed.
