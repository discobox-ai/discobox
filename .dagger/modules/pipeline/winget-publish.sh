#!/usr/bin/env bash
# Manual winget submission; never called by automatic release/promotion.
set -eu
tag="${RELEASE_VERSION:?version required}"
case "$tag" in
  v*) ;;
  *) echo "winget:publish: expected a v* tag, got '$tag'" >&2; exit 1 ;;
esac
# Fail on the token here rather than on the first write it cannot make.
# A token that may read but not write gets all the way through: the
# fork-existence check is a GET, and so is every lookup against
# winget-pkgs, which is public and needs no token at all. The first blob
# POST is where it stops, and GitHub answers an unauthorised write with
# 404 rather than 403 so as not to confirm the repository exists — so
# the failure reads as "the fork is missing", which it is not, and the
# error can never say this for itself.
#
# A fine-grained PAT reports no scopes at all, which is the same empty
# string as a classic token carrying none, and neither can do this.
scopes="$(gh api -i user 2>/dev/null | tr -d "\r" | sed -n "s/^[Xx]-[Oo][Aa]uth-[Ss]copes: *//p")"
case ",$(printf %s "$scopes" | tr -d " ")," in
  *,repo,*|*,public_repo,*) ;;
  *)
    echo "winget:publish: WINGET_TOKEN may not write to the winget-pkgs fork." >&2
    echo "  scopes reported: ${scopes:-(none, which is also what a fine-grained token reports)}" >&2
    echo "  It must be a classic PAT with public_repo (or repo), owned by the account whose" >&2
    echo "  fork of microsoft/winget-pkgs the pull request comes from." >&2
    exit 1 ;;
esac
set -eu
tag="${RELEASE_VERSION:?version required}"
upstream="microsoft/winget-pkgs"
manifests="${1:-/winget}"

# Read back out of what was just generated rather than restated here.
# The identifier and the rule that strips the `v` are the script's to
# own, and a second copy of either is a second copy that can be wrong.
installer="$(ls "$manifests"/*.installer.yaml)"
identifier="$(sed -n 's/^PackageIdentifier: //p' "$installer")"
version="$(sed -n 's/^PackageVersion: //p' "$installer")"

# manifests/<publisher's first letter, lowercased>/<publisher>/<package>.
publisher="${identifier%%.*}"
package="${identifier#*.}"
letter="$(printf %s "$publisher" | cut -c1 | tr '[:upper:]' '[:lower:]')"
package_dir="manifests/$letter/$publisher/$package"

if gh api "repos/$upstream/contents/$package_dir/$version" >/dev/null 2>&1; then
  echo "winget already serves $version"
  exit 0
fi

login="$(gh api user -q .login)"
fork="$login/winget-pkgs"
# Idempotent: forking an existing fork says so and succeeds. No
# --remote: gh rejects the flag outright when a repository argument is
# given, and adds no remote in that case anyway.
gh repo fork "$upstream" --clone=false >/dev/null

# The fork's own master, which is allowed to be thousands of commits
# behind upstream's. It is a clean ancestor rather than a divergence —
# nothing ever commits to it — so it is the pull request's merge base
# and the diff is exactly the files added here. A new version directory
# cannot conflict with whatever upstream changed meanwhile, so the
# staleness costs nothing.
#
# Two things that do not work, both tried:
#
#   `gh repo sync "$fork" --branch master --force` goes through the
#   merge-upstream API. That refuses any incoming
#   commit touching .github/workflows unless the token carries
#   `workflow` scope, and winget-pkgs changes its workflows constantly —
#   so it broke as soon as the fork had any catching up to do. Granting
#   the scope would buy write access to every workflow in every public
#   repository the submitting account can reach, to tidy a fork whose
#   staleness does not matter.
#
#   Reading the base from *upstream* instead, to skip the fork's master
#   entirely, fails further along: a fork shares object storage with its
#   upstream for reads, but tree creation validates base_tree against
#   the repository being written, so `POST repos/$fork/git/trees`
#   answers "base_tree is not a valid tree oid" (422) for a tree only
#   upstream has.
base="$(gh api "repos/$fork/git/ref/heads/master" -q .object.sha)"
base_tree="$(gh api "repos/$fork/git/commits/$base" -q .tree.sha)"

# The title format the repository's pull request template asks for; the
# moderation tooling reads it.
# The two titles winget-pkgs actually uses, and the moderation tooling
# reads them: "New package: <id> version <v>" the first time a package
# appears, "New version: <id> version <v>" every time after. Measured
# rather than guessed — of the last hundred pull requests there, 55 are
# "New version:" and 10 "New package:", against 4 using "Update:", which
# is what this said until the first submission proved it had never run
# down this branch.
if gh api "repos/$upstream/contents/$package_dir" >/dev/null 2>&1; then
  title="New version: $identifier version $version"
else
  title="New package: $identifier version $version"
fi

tree='[]'
for manifest in "$manifests"/*.yaml; do
  blob="$(jq -Rs '{content: ., encoding: "utf-8"}' < "$manifest" |
    gh api "repos/$fork/git/blobs" --input - -q .sha)"
  tree="$(printf %s "$tree" | jq \
    --arg path "$package_dir/$version/$(basename "$manifest")" --arg sha "$blob" \
    '. + [{path: $path, mode: "100644", type: "blob", sha: $sha}]')"
done

new_tree="$(jq -n --arg base "$base_tree" --argjson tree "$tree" \
  '{base_tree: $base, tree: $tree}' |
  gh api "repos/$fork/git/trees" --input - -q .sha)"
commit="$(jq -n --arg message "$title" --arg tree "$new_tree" --arg parent "$base" \
  '{message: $message, tree: $tree, parents: [$parent]}' |
  gh api "repos/$fork/git/commits" --input - -q .sha)"

branch="discobox-$version"
# Force, so a first attempt that failed before the pull request existed
# can simply be run again.
jq -n --arg sha "$commit" '{sha: $sha, force: true}' |
  gh api -X PATCH "repos/$fork/git/refs/heads/$branch" --input - >/dev/null 2>&1 ||
  jq -n --arg ref "refs/heads/$branch" --arg sha "$commit" '{ref: $ref, sha: $sha}' |
    gh api "repos/$fork/git/refs" --input - >/dev/null

existing="$(gh pr list --repo "$upstream" --head "$branch" --state open \
  --json url -q '.[0].url' 2>/dev/null || true)"
if [ -n "$existing" ]; then
  echo "updated the open pull request: $existing"
  exit 0
fi

# Which archives the submission actually offers, read back out of the
# manifest rather than named here. The installer manifest carries one
# entry per windows zip the release uploaded, so an architecture added
# to the build matrix is described correctly without an edit — this
# body claimed amd64 alone for the whole of v0.3.0, which shipped both.

# shellcheck disable=SC2016 # These backticks format Markdown, not shell commands.
archives="$(sed -n 's|.*InstallerUrl: .*/\(discobox-windows-[^/]*\.zip\)$|`\1`|p' "$installer" | paste -sd, - | sed 's/,/, /g')"

# The manifests are generated from the release's own assets and each
# archive was read back to check it holds what they declare, but
# `winget validate` and `winget install --manifest` are Windows-only
# and did not run. Said in the body rather than left as two ticked
# boxes that are not true.
body=/tmp/winget-pr-body.md
printf '%s\n' \
  "Generated from the assets $tag uploaded by the Discobox Dagger pipeline." \
  "" \
  "Installer: $identifier $version, portable, from $archives." \
  "Each checksum names the downloaded asset; every archive contains discobox.exe." \
  "Native Windows validation is stubbed and was not performed." > "$body"
gh pr create --repo "$upstream" --base master --head "$login:$branch" \
  --title "$title" --body-file "$body"
