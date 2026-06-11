#!/usr/bin/env bats

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"

  export DISCO2_BATS_TMP="$BATS_SUITE_TMPDIR/disco2-dockervm"
  export DISCO2_BATS_DATA_DIR="$DISCO2_BATS_TMP/data"
  export DISCO2_BATS_CONFIG_DIR="$DISCO2_BATS_TMP/config"
  export DISCO2_BATS_CACHE_DIR="$DISCO2_BATS_TMP/cache"
  export DISCO2_BATS_STATE_DIR="$DISCO2_BATS_TMP/state"
  export DISCO2_BATS_DB="$DISCO2_BATS_TMP/disco2.sqlite"
  export DISCO2_BATS_TENANT_DB="$DISCO2_BATS_TMP/disco2.00000000-0000-0000-0000-000000000000.sqlite"
  export DISCO2_BATS_SERVER_LOG="$DISCO2_BATS_TMP/server.log"
  mkdir -p "$DISCO2_BATS_DATA_DIR" "$DISCO2_BATS_CONFIG_DIR" "$DISCO2_BATS_CACHE_DIR" "$DISCO2_BATS_STATE_DIR"

  export DISCO2_BATS_PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
  export DISCO2_BATS_SERVER="http://127.0.0.1:$DISCO2_BATS_PORT"

  go build -o build/discobot-server ./cmd/discobot-server
  go build -o build/disco2 ./cmd/disco2
  docker build -f Dockerfile.worker-agent -t disco2-worker-agent:local .

  PORT="$DISCO2_BATS_PORT" \
  DATABASE_DSN="$DISCO2_BATS_DB" \
  DISCO2_DATA_DIR="$DISCO2_BATS_DATA_DIR" \
  DISCO2_CONFIG_DIR="$DISCO2_BATS_CONFIG_DIR" \
  DISCO2_CACHE_DIR="$DISCO2_BATS_CACHE_DIR" \
  DISCO2_STATE_DIR="$DISCO2_BATS_STATE_DIR" \
  DISCO2_TENANT_ID="00000000-0000-0000-0000-000000000000" \
  DISPATCHER_ENABLED=true \
  DISPATCHER_POLL_INTERVAL=200ms \
  DISPATCHER_IMMEDIATE_EXECUTION=true \
    ./build/discobot-server >"$DISCO2_BATS_SERVER_LOG" 2>&1 &
  export DISCO2_BATS_SERVER_PID="$!"

  for _ in {1..100}; do
    if curl -fsS "$DISCO2_BATS_SERVER/openapi.json" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$DISCO2_BATS_SERVER_PID" 2>/dev/null; then
      cat "$DISCO2_BATS_SERVER_LOG" >&2 || true
      return 1
    fi
    sleep 0.1
  done

  cat "$DISCO2_BATS_SERVER_LOG" >&2 || true
  return 1
}

teardown_file() {
  cd "$REPO_ROOT"
  if [ -n "${DISCO2_BATS_SERVER_PID:-}" ] && kill -0 "$DISCO2_BATS_SERVER_PID" 2>/dev/null; then
    kill "$DISCO2_BATS_SERVER_PID" 2>/dev/null || true
    wait "$DISCO2_BATS_SERVER_PID" 2>/dev/null || true
  fi

  docker rm -f $(docker ps -aq \
    --filter "ancestor=disco2-worker-agent:local" \
    --filter "label=disco2.provider_type=dockervm" \
    --filter "label=disco2.project_id=00000000-0000-0000-0000-000000000002") >/dev/null 2>&1 || true
}

cli() {
  "$REPO_ROOT/build/disco2" --server "$DISCO2_BATS_SERVER" --project local --output json "$@"
}

json_get() {
  python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"
}

wait_for_worker_ready() {
  local provider_id="$1"
  if python3 - "$DISCO2_BATS_TENANT_DB" "$provider_id" <<'PY'
import sqlite3
import sys
import time

db, provider_id = sys.argv[1:]
deadline = time.time() + 90
last = None
while time.time() < deadline:
    con = sqlite3.connect(db)
    con.row_factory = sqlite3.Row
    rows = con.execute(
        """
        SELECT id, ready, schedulable, phase, last_operation_status
        FROM workers
        WHERE project_id = ? AND provider_instance_id = ?
        ORDER BY created_at ASC
        """,
        ("00000000-0000-0000-0000-000000000002", provider_id),
    ).fetchall()
    con.close()
    last = [dict(row) for row in rows]
    if any(row["ready"] and row["schedulable"] for row in rows):
        print(rows[0]["id"])
        sys.exit(0)
    time.sleep(1)
print(f"worker did not become ready for provider {provider_id}: {last}", file=sys.stderr)
sys.exit(1)
PY
  then
    return 0
  fi
  echo "server log:" >&2
  tail -200 "$DISCO2_BATS_SERVER_LOG" >&2 || true
  echo "dockervm containers:" >&2
  docker ps -a \
    --filter "label=disco2.provider_instance_id=$provider_id" \
    --format 'table {{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Names}}' >&2 || true
  for container in $(docker ps -aq --filter "label=disco2.provider_instance_id=$provider_id"); do
    echo "logs for $container:" >&2
    docker logs "$container" >&2 || true
    echo "systemctl status for $container:" >&2
    docker exec "$container" systemctl --no-pager status disco2-worker-agent.service >&2 || true
    echo "journal for $container:" >&2
    docker exec "$container" journalctl --no-pager -u disco2-worker-agent.service >&2 || true
  done
  return 1
}

host_gateway() {
  docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}'
}

@test "provider create help exposes dockervm dynamic flags" {
  run cli provider create --help=dockervm
  [ "$status" -eq 0 ]
  [[ "$output" == *"Create a Docker VM provider instance"* ]]
  [[ "$output" == *"--control-plane-url"* ]]
  [[ "$output" == *"--pool-size"* ]]
}

@test "provider create and sandbox create work with dockervm" {
  local gateway control_plane config provider_json provider_id sandbox_json sandbox_id worker_id
  gateway="$(host_gateway)"
  control_plane="http://$gateway:$DISCO2_BATS_PORT"
  config="$(python3 - <<PY
import json
print(json.dumps({
    "controlPlaneUrl": "$control_plane",
    "image": "disco2-worker-agent:local",
    "poolSize": 1,
    "systemd": True,
    "privileged": True,
    "cgroupNsMode": "host",
    "agentPort": 3002,
}))
PY
)"

  run cli provider create --type dockervm --name bats-dockervm --config "$config"
  [ "$status" -eq 0 ]
  provider_json="$output"
  provider_id="$(printf '%s' "$provider_json" | json_get id)"
  [ -n "$provider_id" ]
  [ "$(printf '%s' "$provider_json" | json_get type)" = "dockervm" ]

  worker_id="$(wait_for_worker_ready "$provider_id")"
  [ -n "$worker_id" ]

  run cli sandbox create --name bats-dockervm-sandbox --provider-instance "$provider_id" --wait --wait-timeout 90s
  [ "$status" -eq 0 ]
  sandbox_json="$output"
  sandbox_id="$(printf '%s' "$sandbox_json" | json_get id)"
  [ -n "$sandbox_id" ]
  [ "$(printf '%s' "$sandbox_json" | json_get providerInstanceId)" = "$provider_id" ]
  [ "$(printf '%s' "$sandbox_json" | json_get phase)" = "running" ]
  [ "$(printf '%s' "$sandbox_json" | json_get lastOperationStatus)" = "success" ]
  [[ "$sandbox_json" == *"warm-worker"* ]]
}
