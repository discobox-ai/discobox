#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats runs tests and hooks under set -e, so a failed cd already fails.
#
# A discobox clones its own sources and fetches its origin over Git HTTP from
# its pool (ADR 0126 §4, ADR 26-10-08-561): no origin is bound into it and the
# pool runs no git in its checkout. Against a real Docker pool and real
# sandboxes, this drives what a person sees of that — `git fetch origin`
# inside the discobox sees the developer's new commit, a branch the developer
# did not share is not fetchable, `discobox apply` works, a linked worktree
# and a submodule are delivered through the bare origin instead, and a
# rebuild does not clone over the work in the checkout.
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
  # The CLI is this host as the development server knows it: a source is
  # served live only to a client on the pool's own host, and the server
  # decides that by the client's host ID, which the CLI keeps under the HOME
  # this suite replaces.
  local host_id_file="${XDG_CONFIG_HOME:-$HOME/.config}/discobox/host-id"
  [ -s "$host_id_file" ] || skip "no host ID at $host_id_file to share with the development server"
  DISCOBOX_HOST_ID="$(cat "$host_id_file")"
  export DISCOBOX_HOST_ID
  : >"$BATS_SUITE_TMPDIR/sandboxes"
  : >"$BATS_SUITE_TMPDIR/work"

  cli admin harness ls -o json 2>/dev/null | jq -e '.harnessConfigs[] | select(.slug == "shell" and .configured)' >/dev/null ||
    skip "the shell harness is not configured on the development server"

  # The developer's repository, with a branch it never names to the discobox.
  # Under $HOME, which is what a development pool can see of this host
  # (DISCOBOX_POOL_HOST_MOUNT_PREFIX): one it cannot see is pushed instead,
  # and served from the bare origin, which is not live.
  DISCOBOX_BATS_WORK="$(mktemp -d "$HOME/.discobox-bats-origins.XXXXXX")"
  export DISCOBOX_BATS_WORK
  echo "$DISCOBOX_BATS_WORK" >"$BATS_SUITE_TMPDIR/work"
  export DISCOBOX_BATS_LOCAL="$DISCOBOX_BATS_WORK/local"
  git init -q -b main "$DISCOBOX_BATS_LOCAL"
  git -C "$DISCOBOX_BATS_LOCAL" config user.name Bats
  git -C "$DISCOBOX_BATS_LOCAL" config user.email bats@example.com
  echo one >"$DISCOBOX_BATS_LOCAL/README.md"
  git -C "$DISCOBOX_BATS_LOCAL" add README.md
  commit_in "$DISCOBOX_BATS_LOCAL" one
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

# teardown_file purges the discoboxes this suite made, by their recorded IDs,
# and removes the repositories it made under $HOME.
teardown_file() {
  cd "$REPO_ROOT"
  local id work
  while read -r id; do
    [ -n "$id" ] || continue
    cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
  done <"$BATS_SUITE_TMPDIR/sandboxes"
  work="$(cat "$BATS_SUITE_TMPDIR/work" 2>/dev/null || true)"
  case "$work" in
  "$HOME"/.discobox-bats-origins.*) rm -rf "$work" ;;
  esac
}

cli() {
  HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" "$@"
}

commit_in() {
  git -C "$1" -c user.name=Bats -c user.email=bats@example.com commit -q -m "$2"
}

# new_box makes a discobox from the repository at $1, records it for teardown,
# waits for it, and prints its ID.
new_box() {
  local created id
  created="$(cd "$1" && cli new -H shell -d --include-dirty=false -o json)" ||
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

@test "the discobox's origin is its pool's Git route, and nothing is bound" {
  run in_box 'git remote get-url origin; test -e /.discobox/origins && echo bound || echo unbound'
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "${lines[0]}" == https://git.discobox.internal/*/sandboxes/"$DISCOBOX_BATS_SANDBOX_ID"/git-origins/primary.git ]]
  [ "${lines[1]}" = unbound ]

  run docker inspect --format '{{range .Mounts}}{{println .Destination}}{{end}}' "$(sandbox_container "$DISCOBOX_BATS_SANDBOX_ID")"
  [ "$status" -eq 0 ]
  [[ "$output" != *"/.discobox/origins"* ]]
}

@test "git fetch origin in the discobox sees the developer's new commit" {
  echo two >>"$DISCOBOX_BATS_LOCAL/README.md"
  git -C "$DISCOBOX_BATS_LOCAL" add README.md
  commit_in "$DISCOBOX_BATS_LOCAL" two
  local want
  want="$(git -C "$DISCOBOX_BATS_LOCAL" rev-parse HEAD)"

  run cli admin box get "$DISCOBOX_BATS_SANDBOX_ID" -o json
  [ "$(printf '%s' "$output" | jq -r .config.source.delivery)" = clone ]

  run in_box 'git fetch -q origin && git rev-parse origin/main'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[-1]}" = "$want" ]
}

@test "a branch the developer did not share is not fetchable, by name or by object" {
  run in_box 'git ls-remote origin'
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "$output" == *refs/heads/main* ]]
  [[ "$output" != *private* ]]

  run in_box 'git fetch -q origin private'
  echo "$output"
  [ "$status" -ne 0 ]
  run in_box "git fetch -q origin $DISCOBOX_BATS_PRIVATE"
  echo "$output"
  [ "$status" -ne 0 ]
  run in_box "git cat-file -e $DISCOBOX_BATS_PRIVATE"
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

@test "a linked worktree is delivered through the bare origin" {
  local worktree="$DISCOBOX_BATS_WORK/linked"
  git -C "$DISCOBOX_BATS_LOCAL" worktree add -q -b linked "$worktree" main
  echo linked >"$worktree/linked.txt"
  git -C "$worktree" add linked.txt
  commit_in "$worktree" linked
  local want id
  want="$(git -C "$worktree" rev-parse HEAD)"
  id="$(new_box "$worktree")"

  run in_box "$id" 'git rev-parse HEAD && cat linked.txt && git remote get-url origin && git fetch -q origin && echo fetched'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$want" ]
  [ "${lines[1]}" = linked ]
  [[ "${lines[2]}" == https://git.discobox.internal/*/sandboxes/"$id"/git-origins/primary.git ]]
  [ "${lines[3]}" = fetched ]
}

@test "a submodule checkout is delivered through the bare origin" {
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

  run in_box "$id" 'git rev-parse HEAD && cat sub.txt && git remote get-url origin'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$want" ]
  [ "${lines[1]}" = sub ]
  [[ "${lines[2]}" == https://git.discobox.internal/*/sandboxes/"$id"/git-origins/primary.git ]]
}
