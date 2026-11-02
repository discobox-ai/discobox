# 26-11-02-173 — CI enters the Nix devShell only on Linux

Status: Accepted (narrows [0066](0066-the-build-is-nix-plus-taskfile-and-github-actions-only-triggers-it.md) §1's one job body and §§2, 4's Nix on every runner but Windows)
Date: 2026-11-02

## Context

ADR 0066 §1 made every CI job body `nix develop -c go tool task <target>`, and
§4 made Windows the one exception, because Nix does not run there. The macOS job
(`ci:darwin`, and the release workflow's darwin build) therefore installs Nix
and enters the devShell like Linux does.

On macOS the devShell supplies almost nothing that job uses. `ci:darwin` is
`check:darwin` — `scripts/lint-darwin.sh` with golangci-lint, and `go vet` — and
`release:build:darwin`, a cgo build against Virtualization.framework signed by
`/usr/bin/codesign`. Its tools are Go, the `go tool` dependencies `go.mod`
pins, git, and Xcode's toolchain and `bash`/`comm`/`awk`/`sort`, which the
runner ships. The devShell's Go is only a bootstrap: `GOTOOLCHAIN=auto` fetches
the one `go.mod` names (0066 §3). Its compiler is deliberately not used on
darwin (`mkShellNoCC`, 0066 §3). Nothing else it holds — bats, shellcheck,
node, Docker, PowerShell — is reached by those targets.

It is also the most expensive place to enter it. Depot's macOS runner is the
slowest CI has at anything that writes many files: installing Nix took ~30s and
fetching the devShell ~53s, against ~2s and ~10s on Linux. Measured on PR #443
with Depot Cache warm, the darwin job's setup was 83s with Nix and 10–14s with
`actions/setup-go`, the rest of the job unchanged and every step passing —
darwin lint, `go vet`, and the signed release build.

## Decision

1. **Only Linux enters the devShell in CI.** `.github/actions/task` installs Nix
   and runs `nix develop -c go tool task <target>` on Linux runners. macOS and
   Windows runners take the path 0066 §4 gave Windows: `actions/setup-go` with
   `go-version-file: go.mod`, then `go tool task <target>`.
2. **The darwin devShell stays.** It is for developers on a Mac, whose
   `task dev`, hooks and Bats tests do use what it holds. Only CI stops entering
   it.
3. **A target the macOS or Windows job runs needs nothing but Go.** It may use
   Go, `go tool` dependencies, git, and what the runner image ships. A target
   that needs a devShell tool runs on Linux, or the job that needs it moves
   there.

## Alternatives considered

- **Keep Nix on macOS and cache `/nix/store`** (`nix-community/cache-nix-action`
  or similar). Rejected: the store would be restored through `actions/cache`,
  which on this runner moved ~30 MB/s (8.5 GB in ~288s), so restoring a
  devShell closure costs about what fetching it from cache.nixos.org does, and
  adds keying and pruning to maintain. A per-path cache (Magic Nix Cache) is
  better shaped but still pays the ~30s install for a bootstrap Go.
- **A smaller darwin-only devShell.** Rejected: it still pays the Nix install,
  and a second shell to keep in step with the first for one bootstrap Go is
  the drift 0066 §3 moved away from mise to avoid.
- **Keep the toolchain guarantee on every runner.** 0066 §1's point is that a
  target runs the same in a terminal and in CI. That holds for a step's
  *commands*, which §1 says is what matters; the tools those commands call on
  macOS were already the runner's (Xcode, codesign) or pinned by `go.mod`. The
  guarantee macOS gives up is only for devShell tools no darwin target uses,
  and §3 of this decision keeps it that way.

## Consequences

- The macOS job's setup drops from ~83s to ~12s, on the slowest runner and the
  job that bounds a release.
- Windows and macOS share one setup path in the action, so a change to it is
  exercised on both.
- A darwin target that starts calling a devShell tool fails in CI rather than
  silently using the runner image's version only if that tool is absent from
  the image; when present, it runs unpinned. `REVIEW.md` carries §3 so a review
  catches it.
- 0066 §§1, 2 and 4 otherwise stand: Linux jobs are still one line through
  the devShell, the composite action still carries the repetition, and the
  Windows binary is still cross-compiled from Linux.
