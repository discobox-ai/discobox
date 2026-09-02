#!/usr/bin/env bats
#
# End-to-end coverage of ADR 0024 (SSH is a control-plane ingress onto execs)
# and ADR 0057 (the server binds no SSH port): a stock `ssh`/`scp` client, with
# no Discobox-specific software of its own, reaching a real sandbox through
# `GET /ssh/connect` by way of the `ProxyCommand` an ssh_config carries.
#
# This is the one suite that proves the whole session-mapping chain works
# against the real sandbox-agent/pool-agent, not mocks: SSH session-channel
# requests (pty-req/shell/exec/subsystem) really do turn into execs, and the
# exec's frame-based stdout/stderr/exit really do turn back into SSH data and
# an exit-status.
#
# Builds and starts its own server (like sandbox_upgrade.bats) rather than
# requiring `task dev`, so it owns its database, pool, and enrolled key.

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"
  command -v ssh >/dev/null 2>&1 || skip "ssh client is required"
  command -v scp >/dev/null 2>&1 || skip "scp client is required"
  command -v ssh-keygen >/dev/null 2>&1 || skip "ssh-keygen is required"

  export DISCOBOX_BATS_TMP="$BATS_SUITE_TMPDIR/discobox-ssh"
  export DISCOBOX_BATS_DATA_DIR="$DISCOBOX_BATS_TMP/data"
  export DISCOBOX_BATS_CONFIG_DIR="$DISCOBOX_BATS_TMP/config"
  export DISCOBOX_BATS_CACHE_DIR="$DISCOBOX_BATS_TMP/cache"
  export DISCOBOX_BATS_STATE_DIR="$DISCOBOX_BATS_TMP/state"
  export DISCOBOX_BATS_DB="$DISCOBOX_BATS_TMP/discobox.sqlite"
  export DISCOBOX_BATS_SERVER_LOG="$DISCOBOX_BATS_TMP/server.log"
  export DISCOBOX_BATS_KNOWN_HOSTS="$DISCOBOX_BATS_TMP/known_hosts"
  mkdir -p "$DISCOBOX_BATS_DATA_DIR" "$DISCOBOX_BATS_CONFIG_DIR" "$DISCOBOX_BATS_CACHE_DIR" "$DISCOBOX_BATS_STATE_DIR"
  : >"$DISCOBOX_BATS_KNOWN_HOSTS"

  export DISCOBOX_BATS_PORT="$(free_port)"
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:$DISCOBOX_BATS_PORT"
  export DISCOBOX_BATS_SOCKET="$DISCOBOX_BATS_TMP/server.sock"

  # A throwaway keypair enrolled as a project-scoped SSH key (ADR 0024 §5),
  # not the server-wide authorized_keys file, so this suite also exercises
  # the project-scoped auth layer end to end.
  export DISCOBOX_BATS_SSH_KEY="$DISCOBOX_BATS_TMP/id_ed25519"
  ssh-keygen -t ed25519 -N "" -f "$DISCOBOX_BATS_SSH_KEY" -C "bats@ssh-suite" >/dev/null

  (cd server && go build -o ../build/discobox-server ./cmd/discobox-server)
  rm -f build/discobox
  (cd cli && go build -o ../build/discobox ./cmd/discobox)
  # Through the Taskfile rather than docker directly: both agent images are now
  # built FROM a shared base image, and these targets are what know to build it
  # first.
  go tool task build:pool-agent-image
  go tool task build:sandbox-agent-image
  # A discobox cannot be created without a harness, and a harness is not
  # selectable until its configure flow has run, so this suite needs the shared
  # stub the other end-to-end files use. Nothing here tests harnesses; it is the
  # price of having a real sandbox to reach over SSH.
  (DISCOBOX_DEFAULT_SANDBOX_IMAGE=discobox-sandbox-agent:local go tool task build:harness-stub-image)

  PORT="$DISCOBOX_BATS_PORT" \
  DISCOBOX_SERVER_LISTEN="unix://$DISCOBOX_BATS_SOCKET,http://127.0.0.1:$DISCOBOX_BATS_PORT" \
  DATABASE_DSN="$DISCOBOX_BATS_DB" \
  DISCOBOX_DATA_DIR="$DISCOBOX_BATS_DATA_DIR" \
  DISCOBOX_CONFIG_DIR="$DISCOBOX_BATS_CONFIG_DIR" \
  DISCOBOX_CACHE_DIR="$DISCOBOX_BATS_CACHE_DIR" \
  DISCOBOX_STATE_DIR="$DISCOBOX_BATS_STATE_DIR" \
  DISCOBOX_DOCKER_POOL_IMAGE=discobox-pool-agent:local \
  DISCOBOX_DEFAULT_SANDBOX_IMAGE=discobox-sandbox-agent:local \
  DISPATCHER_ENABLED=true \
  DISPATCHER_POLL_INTERVAL=200ms \
  DISPATCHER_IMMEDIATE_EXECUTION=true \
    ./build/discobox-server >"$DISCOBOX_BATS_SERVER_LOG" 2>&1 &
  export DISCOBOX_BATS_SERVER_PID="$!"

  for _ in {1..100}; do
    if curl -fsS "$DISCOBOX_BATS_SERVER/openapi.yaml" >/dev/null 2>&1; then
      break
    fi
    if ! kill -0 "$DISCOBOX_BATS_SERVER_PID" 2>/dev/null; then
      cat "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
      skip "server failed to start"
    fi
    sleep 0.1
  done

  local pool_id
  pool_id="$(wait_for_seeded_pool)"
  [ -n "$pool_id" ] || skip "no pool was seeded"
  wait_for_pool_ready "$pool_id" || skip "pool did not become ready"

  run cli admin ssh-key add "$DISCOBOX_BATS_SSH_KEY.pub"
  [ "$status" -eq 0 ]

  run cli admin harness create --image discobox-harness-stub:local --slug ssh-stub --name "SSH Stub"
  [ "$status" -eq 0 ]
  run cli admin harness configure ssh-stub </dev/null
  [ "$status" -eq 0 ]

  run cli admin box create --name ssh-e2e --harness ssh-stub --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  export DISCOBOX_BATS_SANDBOX_ID="$(printf '%s' "$output" | json_get id)"
  [ -n "$DISCOBOX_BATS_SANDBOX_ID" ]
}

teardown_file() {
  cd "$REPO_ROOT"
  [ -n "${DISCOBOX_BATS_SANDBOX_ID:-}" ] && cli admin box delete "$DISCOBOX_BATS_SANDBOX_ID" >/dev/null 2>&1 || true

  local pool_ids=""
  if [ -f "$DISCOBOX_BATS_DB" ]; then
    pool_ids="$(python3 -c '
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
print(" ".join(row[0] for row in con.execute("SELECT id FROM pools")))
' "$DISCOBOX_BATS_DB" 2>/dev/null || true)"
  fi
  local pool_id
  for pool_id in $pool_ids; do
    cli admin pool delete "$pool_id" >/dev/null 2>&1 || true
  done
  for _ in {1..30}; do
    local pending=0
    for pool_id in $pool_ids; do
      docker network inspect "discobox-sbnet-$pool_id" >/dev/null 2>&1 && pending=1
    done
    [ "$pending" -eq 0 ] && break
    sleep 1
  done

  if [ -n "${DISCOBOX_BATS_SERVER_PID:-}" ] && kill -0 "$DISCOBOX_BATS_SERVER_PID" 2>/dev/null; then
    kill "$DISCOBOX_BATS_SERVER_PID" 2>/dev/null || true
    wait "$DISCOBOX_BATS_SERVER_PID" 2>/dev/null || true
  fi

  for pool_id in $pool_ids; do
    docker rm -f $(docker ps -aq --filter "label=discobox.pool_id=$pool_id") >/dev/null 2>&1 || true
    docker network rm "discobox-sbnet-$pool_id" >/dev/null 2>&1 || true
  done
}

free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}

cli() {
  "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default --output json "$@"
}

# cli_cp is `discobox cp` against this suite's server. It cannot go through cli:
# cp passes every argument to scp, so the endpoint has to arrive in the
# environment rather than as --server.
cli_cp() {
  DISCOBOX_SERVER="$DISCOBOX_BATS_SERVER" DISCOBOX_PROJECT=default \
    "$REPO_ROOT/build/discobox" cp "$@"
}

json_get() {
  python3 -c '
import json, sys
value = json.load(sys.stdin)
for part in sys.argv[1].split("."):
    value = value.get(part) if isinstance(value, dict) else None
print("" if value is None else value)
' "$1"
}

query() {
  python3 - "$DISCOBOX_BATS_DB" "$1" <<'PY'
import sqlite3
import sys

db, sql = sys.argv[1:]
con = sqlite3.connect(db)
rows = con.execute(sql).fetchall()
con.close()
for row in rows:
    print("\t".join("" if value is None else str(value) for value in row))
PY
}

wait_for_seeded_pool() {
  local pool_id=""
  for _ in {1..60}; do
    pool_id="$(query "SELECT id FROM pools LIMIT 1")"
    [ -n "$pool_id" ] && break
    sleep 1
  done
  printf '%s' "$pool_id"
}

wait_for_pool_ready() {
  local pool_id="$1"
  python3 - "$DISCOBOX_BATS_DB" "$pool_id" <<'PY'
import sqlite3
import sys
import time

db, pool_id = sys.argv[1:]
deadline = time.time() + 90
while time.time() < deadline:
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    rows = con.execute("SELECT ready, schedulable FROM pools WHERE id = ?", (pool_id,)).fetchall()
    con.close()
    if any(row["ready"] and row["schedulable"] for row in rows):
        sys.exit(0)
    time.sleep(1)
sys.exit(1)
PY
}

# ssh_proxy_command is what every stanza this project writes carries, and what
# this suite dials through: the server binds no SSH port, so reaching it means
# running the CLI as a ProxyCommand (ADR 0057).
ssh_proxy_command() {
  printf '%s --server %s admin ssh-proxy' "$REPO_ROOT/build/discobox" "$DISCOBOX_BATS_SERVER"
}

# ssh_client runs an ssh/scp client against this suite's server, trusting its
# freshly generated host key on first connect and pinning it afterward — the
# same accept-new-then-pin a real user's known_hosts does, scoped to this
# run's own file so it never touches a developer's real one.
ssh_client() {
  local cmd="$1"
  shift
  "$cmd" \
    -i "$DISCOBOX_BATS_SSH_KEY" \
    -o ProxyCommand="$(ssh_proxy_command)" \
    -o StrictHostKeyChecking=accept-new \
    -o UserKnownHostsFile="$DISCOBOX_BATS_KNOWN_HOSTS" \
    -o ConnectTimeout=10 \
    "$@"
}

@test "ssh runs a command in the sandbox over the SSH ingress" {
  run ssh_client ssh "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" 'echo hello-from-sandbox'
  [ "$status" -eq 0 ]
  [[ "$output" == *"hello-from-sandbox"* ]]
}

@test "ssh reports the sandbox's real exit code" {
  run ssh_client ssh "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" 'exit 7'
  [ "$status" -eq 7 ]
}

@test "scp round-trips a file through the sftp subsystem" {
  local src="$DISCOBOX_BATS_TMP/upload.txt" remote="/tmp/bats-ssh-upload.txt" dst="$DISCOBOX_BATS_TMP/download.txt"
  echo "sftp round trip $$" >"$src"

  run ssh_client scp "$src" "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1:$remote"
  [ "$status" -eq 0 ]

  run ssh_client ssh "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" "cat $remote"
  [ "$status" -eq 0 ]
  [ "$output" = "$(cat "$src")" ]

  run ssh_client scp "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1:$remote" "$dst"
  [ "$status" -eq 0 ]
  diff "$src" "$dst"
}

# `discobox cp` is the same transfer without the ssh_config: the CLI opens a
# loopback bridge onto GET /ssh/connect for the life of the command, enrolls its
# own key, and rewrites DISCOBOX:PATH into what scp takes.
@test "discobox cp round-trips a file over the CLI's own bridge" {
  local src="$DISCOBOX_BATS_TMP/cp-upload.txt" remote="/tmp/bats-cp-upload.txt" dst="$DISCOBOX_BATS_TMP/cp-download.txt"
  echo "discobox cp round trip $$" >"$src"

  run cli_cp "$src" "$DISCOBOX_BATS_SANDBOX_ID:$remote"
  [ "$status" -eq 0 ]

  run ssh_client ssh "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" "cat $remote"
  [ "$status" -eq 0 ]
  [ "$output" = "$(cat "$src")" ]

  run cli_cp "$DISCOBOX_BATS_SANDBOX_ID:$remote" "$dst"
  [ "$status" -eq 0 ]
  diff "$src" "$dst"
}

# -r is scp's own flag, reaching it untouched, and a relative remote path
# resolves against the discobox user's home rather than a source working tree.
@test "discobox cp copies a directory to a relative remote path" {
  local dir="$DISCOBOX_BATS_TMP/cp-tree"
  mkdir -p "$dir/nested"
  echo one >"$dir/one.txt"
  echo two >"$dir/nested/two.txt"

  run cli_cp -r "$dir" "$DISCOBOX_BATS_SANDBOX_ID:cp-tree"
  [ "$status" -eq 0 ]

  run ssh_client ssh "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" 'cat ~/cp-tree/nested/two.txt'
  [ "$status" -eq 0 ]
  [ "$output" = "two" ]
}

# Nothing to copy to or from a discobox means the command was misread, not that
# scp should be handed a local-to-local copy.
@test "discobox cp refuses a copy that names no discobox" {
  run cli_cp "$DISCOBOX_BATS_TMP/cp-upload.txt" "$DISCOBOX_BATS_TMP/cp-elsewhere.txt"
  [ "$status" -ne 0 ]
  [[ "$output" == *"no discobox was named"* ]]
}

@test "an unenrolled key is refused" {
  local stray="$DISCOBOX_BATS_TMP/id_ed25519_stray"
  ssh-keygen -t ed25519 -N "" -f "$stray" -C "bats@stray" >/dev/null

  run ssh -i "$stray" \
    -o ProxyCommand="$(ssh_proxy_command)" \
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$DISCOBOX_BATS_KNOWN_HOSTS" \
    -o ConnectTimeout=10 -o BatchMode=yes \
    "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" 'echo should-not-run'
  [ "$status" -ne 0 ]
  [[ "$output" != *"should-not-run"* ]]
}
