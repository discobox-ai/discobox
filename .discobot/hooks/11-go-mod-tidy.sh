#!/bin/bash
#---
# name: Go mod tidy
# type: file
# pattern: "{go.mod,go.work,**/go.mod}"
# notify_llm: false
#---

# Keep every module's go.mod/go.sum tidy under BOTH resolution modes so local
# dev (the go.work workspace) and per-module builds (Docker/CI, which run with
# GOWORK=off and -mod=readonly) never drift apart. The workspace papers over a
# stale module go.mod, so drift only surfaces in a no-workspace build (e.g. the
# worker-agent / sandbox-agent Docker images). Tidying each affected module with
# work off *and* on, then syncing the workspace checksums, closes that gap.

set -euo pipefail

workspace="${DISCOBOT_WORKSPACE:-$(pwd)}"
cd "$workspace"

# Authoritative list of module directories, from go.work — avoids tidying stray
# go.mod files that are not part of the workspace. DiskPath is "." or "./cli".
mapfile -t work_dirs < <(go work edit -json | grep -o '"DiskPath": *"[^"]*"' | sed -E 's/.*"DiskPath": *"//; s/"$//; s#^\./##')

# Decide which modules to tidy from the changed files. A changed go.work (or an
# empty change list, i.e. a manual full run) tidies every workspace module.
declare -A tidy_dirs
changed="${DISCOBOT_CHANGED_FILES:-}"
tidy_all=false
[ -z "$changed" ] && tidy_all=true
for f in $changed; do
  f="${f#./}"
  case "$f" in
    go.work|go.work.sum) tidy_all=true ;;
    go.mod|go.sum) tidy_dirs["."]=1 ;;
    */go.mod|*/go.sum) tidy_dirs["$(dirname "$f")"]=1 ;;
  esac
done
if [ "$tidy_all" = true ]; then
  for d in "${work_dirs[@]}"; do tidy_dirs["$d"]=1; done
fi

for d in "${!tidy_dirs[@]}"; do
  [ -f "$d/go.mod" ] || continue
  echo "[go-mod-tidy] tidying ${d}"
  # Standalone (GOWORK=off) tidy is authoritative: the Docker/CI images build
  # each module with GOWORK=off -mod=readonly, so the committed go.mod/go.sum
  # MUST match this resolution. We deliberately do NOT also run a workspace-on
  # tidy on the same module — the workspace resolves shared deps via MVS over
  # the union of all members, producing a *different* go.sum, and running both
  # makes go.sum oscillate between the two resolutions on every file-change.
  # `go mod tidy -diff` is a check mode: it writes nothing and exits non-zero
  # only when an update is actually needed (or resolution fails). We gate every
  # write behind it so an already-tidy module is left untouched — no needless
  # go.sum rewrite (which bumps mtime and churns the file watcher) on unrelated
  # go.mod changes.
  if [ -f "$d/Dockerfile" ]; then
    # Built standalone by a Docker image. A failure here is the exact drift that
    # breaks the image build, so surface it loudly.
    ( cd "$d" && GOWORK=off go mod tidy -diff ) >/dev/null 2>&1 \
      || ( cd "$d" && GOWORK=off go mod tidy )
  elif ( cd "$d" && GOWORK=off go mod tidy -diff ) >/dev/null 2>&1; then
    : # already tidy standalone; nothing to write
  elif ( cd "$d" && GOWORK=off go mod tidy ) 2>/dev/null; then
    : # resolved standalone and wrote the needed changes
  elif ( cd "$d" && go mod tidy -diff ) >/dev/null 2>&1; then
    : # already tidy within the workspace; nothing to write
  else
    # Genuinely workspace-only modules (e.g. cli) depend on unpublished local
    # modules and can only be resolved via go.work. These are never built
    # standalone, so tidy them through the workspace instead.
    ( cd "$d" && go mod tidy ) 2>/dev/null \
      || echo "[go-mod-tidy] note: ${d} is only tidyable within the workspace" >&2
  fi
done

# Reconcile go.work.sum with any updated module requirements. Non-fatal: the
# per-module tidies above are what keep the Docker/CI builds green; a workspace
# sync hiccup should not block the hook.
go work sync || echo "[go-mod-tidy] warning: go work sync reported an issue" >&2
