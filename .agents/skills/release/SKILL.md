---
name: release
description: "Run the release procedure: verify tree and CI state, stage the release locally with dagger, optionally verify the staged artifacts, then push them to GitHub. Driven by `go tool task release` (or release:stage / release:push for the two phases)."
allowed-tools: Bash(git tag, git log, git describe, git push, git branch, git remote, git fetch, git rev-parse, git ls-remote, git for-each-ref, git status, gh run list, gh run view, gh run watch, gh release view, gh release edit, gh api, gh repo view, gh auth status, go tool task, dagger, docker), Read, Glob, Grep, Edit, AskUserQuestion
metadata:
  argument-hint: "[version-or-tag]"
---

# Release Procedure

Releases run **locally**, not in GitHub Actions. CI has read-only permissions
by design: pushes to `main` only run `dagger check` and a staging dry run.
All release logic lives in the dagger module `.dagger/modules/release`; the
Taskfile targets are thin event drivers. Treat `/release` as authorization to
perform the normal flow end-to-end without pausing on the happy path; report
progress briefly.

## The two phases

**`go tool task release:stage`** (`dagger call release stage`) builds the
complete release into `build/release/` for inspection:

- `tag`, `commit` — what the release is cut from (tag inferred as the next
  patch bump of the latest `v*` tag unless `TAG=vX.Y.Z` is set)
- `bin/` — cross-compiled `discobox` CLI binaries
- `images/` — one docker-loadable tarball per agent image and platform

**`go tool task release:push`** (`dagger call release push`) publishes exactly
those staged bytes: creates the tag on GitHub at the staged commit (fails if
the commit is not pushed, or the tag exists at a different commit), publishes
the image tarballs as multi-arch manifests to `ghcr.io/<owner>`, creates the
GitHub release if missing, and uploads the binaries. Idempotent; safe to
re-run.

**`go tool task release`** chains both in one dagger call, gated on
`dagger check` first.

The GitHub repo and registry owner are derived inside the module from the git
`origin` remote (overridable via `[modules.release.settings]` in dagger.toml),
so the same commands work on any fork.

## Version inference

1. If the user provided a version or tag, normalize it to start with `v` and
   pass it as `TAG=`.
2. Otherwise inspect `git tag --sort=-v:refname | head -20` and infer:
   - latest `v1.2.3-alpha4` → `v1.2.3-alpha5`; same pattern for `-betaN` and
     `-rcN`.
   - latest `v1.2.3` (a final release) → start an RC cycle: `v1.2.3-rc1`
     unless the user asked for a final release, then bump patch.
   - no tags → `v0.1.0-rc1`.
3. Show the inferred version and the reason before staging. Pre-release
   versions must be passed explicitly (`TAG=...`) since the module's built-in
   default is a plain patch bump.

## Procedure

1. **Preflight**
   - `git status --short --branch` — working tree clean, on the commit
     intended for release (normally the tip of `main`).
   - `gh auth status` — token needs `repo` and `write:packages` scopes.
   - The commit must be pushed to the GitHub repo (`release push` verifies
     this and fails otherwise) and its CI runs green: `gh run list --commit
     <sha>`, waiting with `gh run watch <run-id> --exit-status`.
2. **Stage**: `TAG=<version> go tool task release:stage` (or omit `TAG`).
   Optionally verify: run a binary from `build/release/bin/`, or
   `docker load < build/release/images/worker-agent-linux-amd64.tar`.
3. **Push**: `go tool task release:push`.
   (Or run both as `TAG=<version> go tool task release`, which also runs
   `dagger check` first.)
4. **Release notes** — once the release exists, generate a short changelog
   from `git log <prev-tag>..<tag> --oneline` and apply it with
   `gh release edit <tag> --notes ...`.
5. **Verify** — `gh release view <tag>` lists the CLI binaries, and
   `docker manifest inspect ghcr.io/<owner>/discobox-worker-agent:<tag>`
   resolves both platforms.

## Failure handling

- If `dagger check` fails, fix the cause when it is clear (or ask the user
  when it is not), then restart.
- If push fails midway, re-run `release:push` — tag creation, release
  creation, and asset upload are all idempotent.
- A "tag exists at a different commit" error means the staged directory is
  stale — re-stage from the right commit; never move a pushed tag without
  explicit user approval.
