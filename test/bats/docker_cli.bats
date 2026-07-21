#!/usr/bin/env bats

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"

  export DISCOBOX_BATS_TMP="$BATS_SUITE_TMPDIR/discobox-docker"
  export DISCOBOX_BATS_DATA_DIR="$DISCOBOX_BATS_TMP/data"
  export DISCOBOX_BATS_CONFIG_DIR="$DISCOBOX_BATS_TMP/config"
  export DISCOBOX_BATS_CACHE_DIR="$DISCOBOX_BATS_TMP/cache"
  export DISCOBOX_BATS_STATE_DIR="$DISCOBOX_BATS_TMP/state"
  export DISCOBOX_BATS_DB="$DISCOBOX_BATS_TMP/discobox.sqlite"
  export DISCOBOX_BATS_SERVER_LOG="$DISCOBOX_BATS_TMP/server.log"
  export DISCOBOX_BATS_POOL_FILE="$DISCOBOX_BATS_TMP/pool-id"
  mkdir -p "$DISCOBOX_BATS_DATA_DIR" "$DISCOBOX_BATS_CONFIG_DIR" "$DISCOBOX_BATS_CACHE_DIR" "$DISCOBOX_BATS_STATE_DIR"

  export DISCOBOX_BATS_PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:$DISCOBOX_BATS_PORT"

  (cd server && go build -o ../build/discobox-server ./cmd/discobox-server)
  rm -f build/disco
  (cd cli && go build -o ../build/disco ./cmd/disco)
  (docker build -f pool-agent/Dockerfile -t discobox-pool-agent:local .)

  PORT="$DISCOBOX_BATS_PORT" \
  DATABASE_DSN="$DISCOBOX_BATS_DB" \
  DISCOBOX_DATA_DIR="$DISCOBOX_BATS_DATA_DIR" \
  DISCOBOX_CONFIG_DIR="$DISCOBOX_BATS_CONFIG_DIR" \
  DISCOBOX_CACHE_DIR="$DISCOBOX_BATS_CACHE_DIR" \
  DISCOBOX_STATE_DIR="$DISCOBOX_BATS_STATE_DIR" \
  DISPATCHER_ENABLED=true \
  DISPATCHER_POLL_INTERVAL=200ms \
  DISPATCHER_IMMEDIATE_EXECUTION=true \
    ./build/discobox-server >"$DISCOBOX_BATS_SERVER_LOG" 2>&1 &
  export DISCOBOX_BATS_SERVER_PID="$!"

  for _ in {1..100}; do
    if curl -fsS "$DISCOBOX_BATS_SERVER/openapi.yaml" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$DISCOBOX_BATS_SERVER_PID" 2>/dev/null; then
      cat "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
      return 1
    fi
    sleep 0.1
  done

  cat "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
  return 1
}

teardown_file() {
  cd "$REPO_ROOT"

  # Delete every pool through the API first, while the server is still up: the
  # control plane owns each pool's Docker network, and nothing else removes it.
  # Leaked networks exhaust Docker's predefined address pools, at which point
  # every later pool fails to reconcile. This sweeps all pools, not just the one
  # this file created, because the server also seeds a default pool at startup.
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

  # Backstop, scoped to this run's pools. Filtering on the ancestor image
  # instead is both too narrow (the project id is generated, never prj_default,
  # so the old filter matched nothing and leaked every pool) and too broad
  # (discobox-pool-agent:local can share an image ID with a developer's own dev
  # tag, so it would reap their running pools).
  for pool_id in $pool_ids; do
    docker rm -f $(docker ps -aq --filter "label=discobox.pool_id=$pool_id") >/dev/null 2>&1 || true
    docker network rm "discobox-sbnet-$pool_id" >/dev/null 2>&1 || true
  done
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

wait_for_pool_ready() {
  local pool_id="$1"
  if python3 - "$DISCOBOX_BATS_DB" "$pool_id" <<'PY'
import sqlite3
import sys
import time

db, pool_id = sys.argv[1:]
deadline = time.time() + 90
last = None
while time.time() < deadline:
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    rows = con.execute(
        """
        SELECT id, ready, schedulable, phase, last_operation_status
        FROM pools
        WHERE id = ?
        """,
        (pool_id,),
    ).fetchall()
    con.close()
    last = [dict(row) for row in rows]
    if any(row["ready"] and row["schedulable"] for row in rows):
        print(rows[0]["id"])
        sys.exit(0)
    time.sleep(1)
print(f"pool did not become ready {pool_id}: {last}", file=sys.stderr)
sys.exit(1)
PY
  then
    return 0
  fi
  echo "server log:" >&2
  tail -200 "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
  echo "docker pool containers:" >&2
  docker ps -a \
    --filter "label=discobox.pool_id=$pool_id" \
    --format 'table {{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Names}}' >&2 || true
  for container in $(docker ps -aq --filter "label=discobox.pool_id=$pool_id"); do
    echo "logs for $container:" >&2
    docker logs "$container" >&2 || true
    echo "systemctl status for $container:" >&2
    docker exec "$container" systemctl --no-pager status discobox-pool-agent.service >&2 || true
    echo "journal for $container:" >&2
    docker exec "$container" journalctl --no-pager -u discobox-pool-agent.service >&2 || true
  done
  return 1
}

host_gateway() {
  docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
}

@test "provider create help exposes docker dynamic flags" {
  run cli box provider create --help=docker
  [ "$status" -eq 0 ]
  [[ "$output" == *"Create a Docker provider instance"* ]]
  [[ "$output" == *"--control-plane-url"* ]]
}

@test "provider create and sandbox create work with docker" {
  local gateway control_plane config provider_json provider_id pool_json pool_id sandbox_json sandbox_id
  gateway="$(host_gateway)"
  control_plane="http://$gateway:$DISCOBOX_BATS_PORT"
  config="$(python3 - <<PY
import json
print(json.dumps({
    "controlPlaneUrl": "$control_plane",
    "image": "discobox-pool-agent:local",
    "systemd": True,
    "privileged": True,
    "cgroupNsMode": "host",
    "agentPort": 3002,
}))
PY
)"

  run cli box provider create --type docker --name bats-docker --config "$config"
  [ "$status" -eq 0 ]
  provider_json="$output"
  provider_id="$(printf '%s' "$provider_json" | json_get id)"
  [ -n "$provider_id" ]
  [ "$(printf '%s' "$provider_json" | json_get type)" = "docker" ]

  run cli box pool create bats-docker-pool --provider "$provider_id"
  [ "$status" -eq 0 ]
  pool_json="$output"
  pool_id="$(printf '%s' "$pool_json" | json_get id)"
  [ -n "$pool_id" ]
  printf '%s' "$pool_id" >"$DISCOBOX_BATS_POOL_FILE"

  pool_id="$(wait_for_pool_ready "$pool_id")"
  [ -n "$pool_id" ]

  run cli box sandbox create --name bats-docker-sandbox --pool "$pool_id" --wait --wait-timeout 90s
  [ "$status" -eq 0 ]
  sandbox_json="$output"
  sandbox_id="$(printf '%s' "$sandbox_json" | json_get id)"
  [ -n "$sandbox_id" ]
  [ "$(printf '%s' "$sandbox_json" | json_get poolId)" = "$pool_id" ]
  [ "$(printf '%s' "$sandbox_json" | json_get runtime.phase)" = "running" ]
  [ "$(printf '%s' "$sandbox_json" | json_get runtime.lastOperationStatus)" = "success" ]
}
