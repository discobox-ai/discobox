#!/usr/bin/env bats
#
# End-to-end coverage of ADR 0024 (SSH is a control-plane ingress onto execs):
# a plain `ssh`/`scp` client, with no Discobox-specific software, reaching a
# real sandbox through discobox-server's SSH listener.
#
# This is the one suite that proves the whole session-mapping chain works
# against the real sandbox-agent/pool-agent, not mocks: SSH session-channel
# requests (pty-req/shell/exec/subsystem) really do turn into execs, and the
# exec's frame-based stdout/stderr/exit really do turn back into SSH data and
# an exit-status.
#
# Builds and starts its own server (like sandbox_upgrade.bats), rather than
# requiring `task dev`, so DISCOBOX_SSH_LISTEN can be set for this run only.

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
  export DISCOBOX_BATS_SSH_PORT="$(free_port)"
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:$DISCOBOX_BATS_PORT"
  export DISCOBOX_BATS_SOCKET="$DISCOBOX_BATS_TMP/server.sock"

  # A throwaway keypair enrolled as a project-scoped SSH key (ADR 0024 §5),
  # not the server-wide authorized_keys file, so this suite also exercises
  # the project-scoped auth layer end to end.
  export DISCOBOX_BATS_SSH_KEY="$DISCOBOX_BATS_TMP/id_ed25519"
  ssh-keygen -t ed25519 -N "" -f "$DISCOBOX_BATS_SSH_KEY" -C "bats@ssh-suite" >/dev/null

  (cd server && go build -o ../build/discobox-server ./cmd/discobox-server)
  rm -f build/disco
  (cd cli && go build -o ../build/disco ./cmd/disco)
  (docker build -f pool-agent/Dockerfile -t discobox-pool-agent:local .)
  (docker build -f sandbox-agent/Dockerfile --target sandbox-agent -t discobox-sandbox-agent:local .)

  PORT="$DISCOBOX_BATS_PORT" \
  DISCOBOX_SERVER_LISTEN="unix://$DISCOBOX_BATS_SOCKET,http://127.0.0.1:$DISCOBOX_BATS_PORT" \
  DISCOBOX_SSH_LISTEN="127.0.0.1:$DISCOBOX_BATS_SSH_PORT" \
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

  run cli ssh-key add "$DISCOBOX_BATS_SSH_KEY.pub"
  [ "$status" -eq 0 ]

  run cli box sandbox create --name ssh-e2e --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  export DISCOBOX_BATS_SANDBOX_ID="$(printf '%s' "$output" | json_get id)"
  [ -n "$DISCOBOX_BATS_SANDBOX_ID" ]
}

teardown_file() {
  cd "$REPO_ROOT"
  [ -n "${DISCOBOX_BATS_SANDBOX_ID:-}" ] && cli box sandbox delete "$DISCOBOX_BATS_SANDBOX_ID" >/dev/null 2>&1 || true

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
    cli box pool delete "$pool_id" >/dev/null 2>&1 || true
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
  "$REPO_ROOT/build/disco" --server "$DISCOBOX_BATS_SERVER" --project default --output json "$@"
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

# ssh_client runs an ssh/scp client against this suite's server, trusting its
# freshly generated host key on first connect and pinning it afterward — the
# same accept-new-then-pin a real user's known_hosts does, scoped to this
# run's own file so it never touches a developer's real one.
ssh_client() {
  local cmd="$1"
  shift
  "$cmd" \
    -i "$DISCOBOX_BATS_SSH_KEY" \
    -p "$DISCOBOX_BATS_SSH_PORT" \
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

@test "an unenrolled key is refused" {
  local stray="$DISCOBOX_BATS_TMP/id_ed25519_stray"
  ssh-keygen -t ed25519 -N "" -f "$stray" -C "bats@stray" >/dev/null

  run ssh -i "$stray" -p "$DISCOBOX_BATS_SSH_PORT" \
    -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="$DISCOBOX_BATS_KNOWN_HOSTS" \
    -o ConnectTimeout=10 -o BatchMode=yes \
    "$DISCOBOX_BATS_SANDBOX_ID@127.0.0.1" 'echo should-not-run'
  [ "$status" -ne 0 ]
  [[ "$output" != *"should-not-run"* ]]
}
