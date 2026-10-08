#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats runs tests and hooks under set -e, so a failed cd already fails.
#
# The worktree Git route end to end (ADR 0126 §4): a discobox's own repository
# is served by the sandbox agent inside it, and the control plane and the pool
# agent only forward to it. `discobox apply` fetches through it, and a client
# clones, pushes and fetches through it with plain git, against a real Docker
# pool and a real sandbox.
#
# Like docker_cli.bats it drives the development stack that is already running
# (`task dev`) and owns only what it creates: one discobox, purged by its
# recorded ID. It skips when the stack is not up. The CLI runs with a HOME of
# its own, so `discobox new` enrolls its SSH key and writes its state there
# rather than into the home of whoever runs the suite.

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

  cli admin harness ls -o json 2>/dev/null | jq -e '.harnessConfigs[] | select(.slug == "shell" and .configured)' >/dev/null ||
    skip "the shell harness is not configured on the development server"

  # The developer's repository the discobox is cut from.
  export DISCOBOX_BATS_LOCAL="$BATS_SUITE_TMPDIR/local"
  git init -q -b main "$DISCOBOX_BATS_LOCAL"
  # apply cherry-picks here, and the global configuration is switched off.
  git -C "$DISCOBOX_BATS_LOCAL" config user.name Bats
  git -C "$DISCOBOX_BATS_LOCAL" config user.email bats@example.com
  echo one >"$DISCOBOX_BATS_LOCAL/README.md"
  git -C "$DISCOBOX_BATS_LOCAL" add README.md
  commit_in "$DISCOBOX_BATS_LOCAL" one

  local created
  created="$(cd "$DISCOBOX_BATS_LOCAL" && cli new -H shell -d --include-dirty=false -o json)" ||
    { echo "$created" >&2; return 1; }
  DISCOBOX_BATS_SANDBOX_ID="$(printf '%s' "$created" | jq -r .id)"
  export DISCOBOX_BATS_SANDBOX_ID
  [ -n "$DISCOBOX_BATS_SANDBOX_ID" ] && [ "$DISCOBOX_BATS_SANDBOX_ID" != null ]
  echo "$DISCOBOX_BATS_SANDBOX_ID" >"$BATS_SUITE_TMPDIR/sandbox"
  wait_for_ready "$DISCOBOX_BATS_SANDBOX_ID"

  local box
  box="$(cli admin box get "$DISCOBOX_BATS_SANDBOX_ID" -o json)"
  DISCOBOX_BATS_PROJECT_ID="$(printf '%s' "$box" | jq -r .projectId)"
  DISCOBOX_BATS_POOL_ID="$(printf '%s' "$box" | jq -r .poolId)"
  export DISCOBOX_BATS_PROJECT_ID DISCOBOX_BATS_POOL_ID
  export DISCOBOX_BATS_URL="$DISCOBOX_BATS_SERVER/projects/$DISCOBOX_BATS_PROJECT_ID/sandboxes/$DISCOBOX_BATS_SANDBOX_ID/git-repositories/primary.git"
}

# teardown_file purges the one discobox this suite made, by its recorded ID.
teardown_file() {
  cd "$REPO_ROOT"
  local id
  id="$(cat "$BATS_SUITE_TMPDIR/sandbox" 2>/dev/null || true)"
  [ -n "$id" ] || return 0
  cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
}

cli() {
  HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" "$@"
}

commit_in() {
  git -C "$1" -c user.name=Bats -c user.email=bats@example.com commit -q -m "$2"
}

# in_box runs a shell command in the discobox's primary source.
in_box() {
  cli shell "$DISCOBOX_BATS_SANDBOX_ID" -- sh -c "$1"
}

wait_for_ready() {
  local id="$1" deadline=$((SECONDS + 180)) state=""
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(cli admin box get "$id" -o json 2>/dev/null | jq -r '.runtime.state // empty')"
    [ "$state" = ready ] && return 0
    sleep 2
  done
  echo "discobox $id did not become ready (last state: $state)" >&2
  return 1
}

sandbox_container() {
  echo "discobox-sandbox-$DISCOBOX_BATS_POOL_ID-$DISCOBOX_BATS_SANDBOX_ID"
}

@test "discobox apply brings back a commit made in the discobox" {
  run in_box 'echo from-box >box.txt && git add box.txt && git -c user.name=Box -c user.email=box@example.com commit -q -m "made in the box" && git rev-parse HEAD'
  [ "$status" -eq 0 ]
  local made="${lines[-1]}"

  run bash -c "cd '$DISCOBOX_BATS_LOCAL' && HOME='$DISCOBOX_BATS_HOME' '$REPO_ROOT/build/discobox' --server '$DISCOBOX_BATS_SERVER' apply '$DISCOBOX_BATS_SANDBOX_ID'"
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "$output" == *"APPLIED 1 commit"* ]]
  [ "$(cat "$DISCOBOX_BATS_LOCAL/box.txt")" = from-box ]
  [ "$(git -C "$DISCOBOX_BATS_LOCAL" log -1 --format=%s)" = "made in the box" ]
  [ -n "$made" ]
}

@test "the worktree route clones, takes a push into the checkout, and fetches" {
  local clone="$BATS_TEST_TMPDIR/clone"
  run git clone -q "$DISCOBOX_BATS_URL" "$clone"
  echo "$output"
  [ "$status" -eq 0 ]

  echo pushed >"$clone/pushed.txt"
  git -C "$clone" add pushed.txt
  commit_in "$clone" "pushed from the client"
  local pushed
  pushed="$(git -C "$clone" rev-parse HEAD)"
  run git -C "$clone" push -q origin main
  echo "$output"
  [ "$status" -eq 0 ]

  # The push lands in the discobox's own checkout, checked out, and owned by
  # the discobox's user rather than by whoever serves the route.
  # shellcheck disable=SC2016 # Expanded by the shell in the discobox, not here.
  run in_box 'git rev-parse HEAD && cat pushed.txt && git status --porcelain | wc -l && [ "$(stat -c %u pushed.txt)" = "$(id -u)" ] && echo owned'
  echo "$output"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "$pushed" ]
  [ "${lines[1]}" = pushed ]
  [ "${lines[2]}" = 0 ]
  [ "${lines[3]}" = owned ]

  run in_box 'echo again >again.txt && git add again.txt && git -c user.name=Box -c user.email=box@example.com commit -q -m again && git rev-parse HEAD'
  [ "$status" -eq 0 ]
  local made="${lines[-1]}"
  run git -C "$clone" fetch -q origin
  [ "$status" -eq 0 ]
  [ "$(git -C "$clone" rev-parse origin/main)" = "$made" ]
}

@test "a slug that names no source is answered by the discobox as not found" {
  run git ls-remote "${DISCOBOX_BATS_URL%/primary.git}/nosuch.git"
  echo "$output"
  [ "$status" -ne 0 ]
  [[ "$output" == *"remote: repository not found: nosuch"* ]]
}

# The sandbox agent authorizes the route on its own (ADR 0126 §5): reaching
# it inside the discobox, past the control plane and the pool, gets nothing
# without a token it accepts. Which scope reads and which pushes is covered
# with signed tokens at each hop by the Go tests.
@test "the sandbox agent refuses the route without a token it accepts" {
  local base="http://127.0.0.1:3003/api/projects/$DISCOBOX_BATS_PROJECT_ID/sandboxes/$DISCOBOX_BATS_SANDBOX_ID/git-repositories/primary.git"
  run docker exec "$(sandbox_container)" curl -s -o /dev/null -w '%{http_code}' "$base/info/refs?service=git-upload-pack"
  [ "$status" -eq 0 ]
  [ "$output" = 401 ]
  run docker exec "$(sandbox_container)" curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer not-a-token' -X POST "$base/git-receive-pack"
  [ "$status" -eq 0 ]
  [ "$output" = 401 ]
}
