#!/usr/bin/env bash
# Recompute both Homebrew channels without a GitHub workflow dispatch.
set -euo pipefail
repo="${GH_REPO:?repository required}"
tap="${TAP_REPOSITORY:?tap required}"
export GIT_CONFIG_COUNT=2
export GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=
export GIT_CONFIG_KEY_1=credential.helper GIT_CONFIG_VALUE_1='!gh auth git-credential'
# Regenerate after each rejected push so two callers cannot overwrite a newer
# tap commit. This never force pushes.
for attempt in 1 2 3; do
  # Preserve the former tap workflow's selection and limits exactly.
  stable=$(gh release list --repo "$repo" --exclude-pre-releases --exclude-drafts --limit 1 --json tagName --jq '.[0].tagName // ""')
  dev=$(gh release list --repo "$repo" --exclude-drafts --limit 30 --json tagName --jq '[.[] | select(.tagName | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))] | .[0].tagName // ""')
  if [ -z "$dev$stable" ]; then echo "No published Homebrew channels"; exit 0; fi
  work=$(mktemp -d)
  git clone --depth 1 "https://github.com/$tap.git" "$work/tap"
  mkdir -p "$work/tap/Formula"
  if [ -n "$dev" ]; then scripts/brew-formula.sh --dev "$dev" "$repo" "$work/tap/Formula/discobox-dev.rb"; fi
  if [ -n "$stable" ]; then scripts/brew-formula.sh "$stable" "$repo" "$work/tap/Formula/discobox.rb"; fi
  git -C "$work/tap" add Formula
  if git -C "$work/tap" diff --cached --quiet; then echo "Homebrew channels are current"; exit 0; fi
  git -C "$work/tap" -c user.name='discobox release' -c user.email=noreply@discobox.ai -c commit.gpgsign=false commit -qm 'Refresh Discobox release channels'
  if git -C "$work/tap" push origin HEAD:main; then echo "Homebrew channels updated"; exit 0; fi
  rm -rf "$work"
  echo "Tap changed concurrently; retry $attempt" >&2
done
echo 'Homebrew update failed after three concurrent-update retries' >&2
exit 1
