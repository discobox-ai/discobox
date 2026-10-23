#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats runs tests and hooks under set -e, so a failed cd already fails.
#
# A discobox's source is delivered by a client into its pool's cache of the
# source's remote, and the discobox clones its own namespace of that cache
# (ADR 26-10-10-545): nothing is bound into it, the pool fetches nothing, and
# no pool reads the developer's directory. Against a real Docker pool and real
# sandboxes, this drives what a person sees of that — the checkout reads as a
# clone of the source's remote, with the pool as its `discobox` remote; a
# commit made after the create shows up only once `discobox push` sends it;
# another discobox's commit in the same cache is not fetchable; `discobox
# apply` works; a linked worktree, a submodule and a remote URL are delivered
# the same way; and a rebuild does not clone over the work in the checkout.
#
# Like worktree_git.bats it drives the development stack that is already
# running (`task dev`) and owns only what it creates, purged by recorded ID.
# It skips when the stack is not up.

setup_file() {
  REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  export REPO_ROOT
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"
  command -v jq >/dev/null 2>&1 || skip "jq is required"

  export DISCOBOX_BATS_PORT="${PORT:-18080}"
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:$DISCOBOX_BATS_PORT"
  curl -fsS "$DISCOBOX_BATS_SERVER/openapi.yaml" >/dev/null 2>&1 ||
    skip "no development server on port $DISCOBOX_BATS_PORT; run 'task dev'"

  (cd cli && go build -o ../build/discobox ./cmd/discobox) || skip "cannot build the CLI"

  export DISCOBOX_BATS_HOME="$BATS_SUITE_TMPDIR/home"
  mkdir -p "$DISCOBOX_BATS_HOME"
  export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
  : >"$BATS_SUITE_TMPDIR/sandboxes"
  : >"$BATS_SUITE_TMPDIR/work"

  cli admin harness ls -o json 2>/dev/null | jq -e '.harnessConfigs[] | select(.slug == "shell" and .configured)' >/dev/null ||
    skip "the shell harness is not configured on the development server"

  # The developer's repository: a checkout of a remote nothing here can
  # reach, one commit ahead of where it last saw that remote, with a branch
  # it never delivers to the suite's discobox.
  DISCOBOX_BATS_WORK="$(mktemp -d "$BATS_SUITE_TMPDIR/origins.XXXXXX")"
  export DISCOBOX_BATS_WORK
  export DISCOBOX_BATS_REMOTE="https://git.invalid/bats/widget-$$.git"
  export DISCOBOX_BATS_LOCAL="$DISCOBOX_BATS_WORK/local"
  git init -q -b main "$DISCOBOX_BATS_LOCAL"
  git -C "$DISCOBOX_BATS_LOCAL" config user.name Bats
  git -C "$DISCOBOX_BATS_LOCAL" config user.email bats@example.com
  git -C "$DISCOBOX_BATS_LOCAL" remote add origin "$DISCOBOX_BATS_REMOTE"
  echo zero >"$DISCOBOX_BATS_LOCAL/README.md"
  git -C "$DISCOBOX_BATS_LOCAL" add README.md
  commit_in "$DISCOBOX_BATS_LOCAL" zero
  DISCOBOX_BATS_PUBLISHED="$(git -C "$DISCOBOX_BATS_LOCAL" rev-parse HEAD)"
  export DISCOBOX_BATS_PUBLISHED
  git -C "$DISCOBOX_BATS_LOCAL" update-ref refs/remotes/origin/main "$DISCOBOX_BATS_PUBLISHED"
  echo one >>"$DISCOBOX_BATS_LOCAL/README.md"
  commit_in "$DISCOBOX_BATS_LOCAL" one -a
  git -C "$DISCOBOX_BATS_LOCAL" checkout -q -b private
  echo private >"$DISCOBOX_BATS_LOCAL/private.txt"
  git -C "$DISCOBOX_BATS_LOCAL" add private.txt
  commit_in "$DISCOBOX_BATS_LOCAL" private
  DISCOBOX_BATS_PRIVATE="$(git -C "$DISCOBOX_BATS_LOCAL" rev-parse HEAD)"
  export DISCOBOX_BATS_PRIVATE
  git -C "$DISCOBOX_BATS_LOCAL" checkout -q main

  DISCOBOX_BATS_SANDBOX_ID="$(new_box "$DISCOBOX_BATS_LOCAL")"
  export DISCOBOX_BATS_SANDBOX_ID
}

# teardown_file purges the discoboxes this suite made, by their recorded IDs.
teardown_file() {
  cd "$REPO_ROOT"
  local id
  while read -r id; do
    [ -n "$id" ] || continue
    cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
  done <"$BATS_SUITE_TMPDIR/sandboxes"
}

cli() {
  HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" "$@"
}

commit_in() {
  local dir="$1" message="$2"
  shift 2
  git -C "$dir" -c user.name=Bats -c user.email=bats@example.com commit -q -m "$message" "$@"
}

# new_box makes a discobox from SOURCE ($2, or . when it is not given), run
# from the directory $1, records it for teardown, waits for it, and prints its
# ID.
new_box() {
  local created id dir="$1" source="${2:-.}"
  created="$(cd "$dir" && cli run -H shell -d -o json "$source")" ||
    { echo "$created" >&2; return 1; }
  id="$(printf '%s' "$created" | jq -r .id)"
  [ -n "$id" ] && [ "$id" != null ]
  echo "$id" >>"$BATS_SUITE_TMPDIR/sandboxes"
  wait_for_ready "$id"
  echo "$id"
}

# in_box runs a shell command in a discobox's primary source: the suite's own
# discobox unless an ID is given first.
in_box() {
  local id="$DISCOBOX_BATS_SANDBOX_ID"
  if [ "$#" -gt 1 ]; then
    id="$1"
    shift
  fi
  cli shell "$id" -- sh -c "$1"
}

wait_for_ready() {
  local id="$1" deadline=$((SECONDS + 300)) state=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(cli admin box get "$id" -o json 2>/dev/null | jq -r '.runtime.state // empty')"
    [ "$state" = ready ] && return 0
    sleep 2
  done
  echo "discobox $id did not become ready (last state: $state)" >&2
  return 1
}

sandbox_container() {
  local box
  box="$(cli admin box get "$1" -o json)"
  echo "discobox-sandbox-$(printf '%s' "$box" | jq -r .poolId)-$1"
}

pool_route() {
  echo "https://git.discobox.internal/*/sandboxes/$1/git-origins/primary.git"
}

@test "the checkout reads as a clone of the source's remote, and the pool is its discobox remote" {
  run cli admin box get "$DISCOBOX_BATS_SANDBOX_ID" -o json
  [ "$(printf '%s' "$output" | jq -r .config.source.delivery)" = cache ]
  [[ "$(printf '%s' "$output" | jq -r .config.source.cacheKey)" == remote/git.invalid/bats/widget-* ]]

  run in_box 'git remote get-url origin; git remote get-url discobox; git remote | sort | tr "\n" " "; echo; git rev-parse origin/main; git rev-parse discobox/main HEAD; test -e /.discobox/origins && echo bound || echo unbound'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$DISCOBOX_BATS_REMOTE" ]
  # shellcheck disable=SC2053 # the right side is a pattern on purpose
  [[ "${lines[1]}" == $(pool_route "$DISCOBOX_BATS_SANDBOX_ID") ]]
  [ "${lines[2]}" = "discobox origin " ]
  # origin/main is where the remote was, not where the developer's unpushed
  # commit is; the pool's branch and the checkout are at that commit.
  [ "${lines[3]}" = "$DISCOBOX_BATS_PUBLISHED" ]
  local head
  head="$(git -C "$DISCOBOX_BATS_LOCAL" rev-parse main)"
  [ "${lines[4]}" = "$head" ]
  [ "${lines[5]}" = "$head" ]
  [ "${lines[6]}" = unbound ]

  run in_box 'git rev-parse --abbrev-ref "main@{upstream}"'
  [ "${lines[0]}" = origin/main ]

  run docker inspect --format '{{range .Mounts}}{{println .Destination}}{{end}}' "$(sandbox_container "$DISCOBOX_BATS_SANDBOX_ID")"
  [ "$status" -eq 0 ]
  [[ "$output" != *"/.discobox/origins"* ]]
}

@test "the discobox sees the developer's new commit once discobox push sends it" {
  local before want
  before="$(git -C "$DISCOBOX_BATS_LOCAL" rev-parse HEAD)"
  echo two >>"$DISCOBOX_BATS_LOCAL/README.md"
  commit_in "$DISCOBOX_BATS_LOCAL" two -a
  want="$(git -C "$DISCOBOX_BATS_LOCAL" rev-parse HEAD)"

  # Nothing follows the developer's directory unattended.
  run in_box 'git fetch -q discobox && git rev-parse discobox/main'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[-1]}" = "$before" ]

  run bash -c "cd '$DISCOBOX_BATS_LOCAL' && HOME='$DISCOBOX_BATS_HOME' '$REPO_ROOT/build/discobox' --server '$DISCOBOX_BATS_SERVER' push '$DISCOBOX_BATS_SANDBOX_ID'"
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "$output" == *"git fetch discobox && git rebase discobox/main"* ]]

  run in_box 'git fetch -q discobox && git rev-parse discobox/main'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[-1]}" = "$want" ]
}

@test "another discobox's commit in the same cache is not fetchable, by name or by object" {
  # A second discobox from the same remote, at the private branch: its
  # commit is now in the cache the first one's namespace shares.
  local other
  other="$(new_box "$DISCOBOX_BATS_LOCAL" "$DISCOBOX_BATS_LOCAL@private")"
  run in_box "$other" 'git rev-parse HEAD'
  echo "$output"
  [ "${lines[0]}" = "$DISCOBOX_BATS_PRIVATE" ]
  run cli admin box get "$other" -o json
  [ "$(printf '%s' "$output" | jq -r .config.source.cacheKey)" = "$(cli admin box get "$DISCOBOX_BATS_SANDBOX_ID" -o json | jq -r .config.source.cacheKey)" ]

  run in_box 'git ls-remote discobox'
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "$output" == *refs/heads/main* ]]
  [[ "$output" != *private* ]]
  [[ "$output" != *"$DISCOBOX_BATS_PRIVATE"* ]]

  run in_box 'git fetch -q discobox private'
  echo "$output"
  [ "$status" -ne 0 ]
  local protocol
  for protocol in 0 2; do
    run in_box "git -c protocol.version=$protocol fetch -q discobox $DISCOBOX_BATS_PRIVATE"
    echo "$output"
    [ "$status" -ne 0 ]
  done
  run in_box "git cat-file -e $DISCOBOX_BATS_PRIVATE"
  [ "$status" -ne 0 ]
  # And it pushes nothing into its own namespace.
  run in_box 'git push -q discobox HEAD:refs/heads/from-the-box'
  echo "$output"
  [ "$status" -ne 0 ]
}

@test "discobox apply brings back a commit made in the discobox" {
  run in_box 'echo from-box >box.txt && git add box.txt && git -c user.name=Box -c user.email=box@example.com commit -q -m "made in the box"'
  [ "$status" -eq 0 ]

  run bash -c "cd '$DISCOBOX_BATS_LOCAL' && HOME='$DISCOBOX_BATS_HOME' '$REPO_ROOT/build/discobox' --server '$DISCOBOX_BATS_SERVER' apply '$DISCOBOX_BATS_SANDBOX_ID'"
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "$output" == *"APPLIED 1 commit"* ]]
  [ "$(cat "$DISCOBOX_BATS_LOCAL/box.txt")" = from-box ]
}

@test "a rebuild does not clone over the work in the checkout" {
  run in_box 'echo kept >uncommitted.txt && cat .git/discobox-materialized && git rev-parse HEAD'
  echo "$output"
  [ "$status" -eq 0 ]
  local marker="${lines[0]}" head="${lines[1]}"

  run cli admin box repair "$DISCOBOX_BATS_SANDBOX_ID"
  echo "$output"
  [ "$status" -eq 0 ]
  wait_for_ready "$DISCOBOX_BATS_SANDBOX_ID"

  run in_box 'cat uncommitted.txt && cat .git/discobox-materialized && git rev-parse HEAD'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = kept ]
  [ "${lines[1]}" = "$marker" ]
  [ "${lines[2]}" = "$head" ]
}

@test "a linked worktree is delivered like any other checkout" {
  local worktree="$DISCOBOX_BATS_WORK/linked"
  git -C "$DISCOBOX_BATS_LOCAL" worktree add -q -b linked "$worktree" main
  echo linked >"$worktree/linked.txt"
  git -C "$worktree" add linked.txt
  commit_in "$worktree" linked
  local want id
  want="$(git -C "$worktree" rev-parse HEAD)"
  id="$(new_box "$worktree")"

  run in_box "$id" 'git rev-parse HEAD && cat linked.txt && git remote get-url discobox && git fetch -q discobox && echo fetched'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$want" ]
  [ "${lines[1]}" = linked ]
  # shellcheck disable=SC2053 # the right side is a pattern on purpose
  [[ "${lines[2]}" == $(pool_route "$id") ]]
  [ "${lines[3]}" = fetched ]
}

@test "a submodule checkout is delivered like any other checkout" {
  local sub="$DISCOBOX_BATS_WORK/sub" super="$DISCOBOX_BATS_WORK/super"
  git init -q -b main "$sub"
  echo sub >"$sub/sub.txt"
  git -C "$sub" add sub.txt
  commit_in "$sub" sub
  git init -q -b main "$super"
  git -C "$super" -c protocol.file.allow=always submodule add -q "$sub" sub
  commit_in "$super" "add sub"
  local want id
  want="$(git -C "$super/sub" rev-parse HEAD)"
  id="$(new_box "$super/sub")"

  run in_box "$id" 'git rev-parse HEAD && cat sub.txt && git remote get-url discobox'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$want" ]
  [ "${lines[1]}" = sub ]
  # shellcheck disable=SC2053 # the right side is a pattern on purpose
  [[ "${lines[2]}" == $(pool_route "$id") ]]
}

@test "a remote URL is delivered by this client, and the discobox clones it from its pool" {
  local served="$DISCOBOX_BATS_WORK/served.git" url want id
  git clone -q --bare "$DISCOBOX_BATS_LOCAL" "$served"
  url="file://$served"
  want="$(git -C "$served" rev-parse refs/heads/main)"
  # HOME is the suite's, so this client's cache of the remote is too.
  id="$(new_box "$DISCOBOX_BATS_WORK" "$url@main")"

  run in_box "$id" 'git rev-parse HEAD origin/main discobox/main && git remote get-url origin && git branch -r | grep -c origin/private'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$want" ]
  [ "${lines[1]}" = "$want" ]
  [ "${lines[2]}" = "$want" ]
  [ "${lines[3]}" = "$url" ]
  # Every branch of the remote is there, as a clone of it would have them.
  [ "${lines[4]}" = 1 ]

  # A second discobox from the same commit needs nothing fetched or pushed.
  id="$(new_box "$DISCOBOX_BATS_WORK" "$url@main")"
  run in_box "$id" 'git rev-parse HEAD'
  [ "${lines[0]}" = "$want" ]
}
