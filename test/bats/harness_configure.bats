#!/usr/bin/env bats
#
# End-to-end coverage of the harness configure flow against the stub harness
# (test/harness-stub). What is unique to this file is the seeding half of the
# contract, which no unit test can reach: that the control plane's seed lands in
# a real sandbox before the configure command runs, that it carries secret
# metadata but never a secret value, and that the value arrives instead as a
# PREV_-prefixed sentinel the command can see.
#
# The pool binds the host Docker socket so locally built images
# (discobox-harness-stub:local) are visible without a registry.

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"

  export DISCOBOX_BATS_TMP="$BATS_SUITE_TMPDIR/discobox-harness-configure"
  export DISCOBOX_BATS_DATA_DIR="$DISCOBOX_BATS_TMP/data"
  export DISCOBOX_BATS_CONFIG_DIR="$DISCOBOX_BATS_TMP/config"
  export DISCOBOX_BATS_CACHE_DIR="$DISCOBOX_BATS_TMP/cache"
  export DISCOBOX_BATS_STATE_DIR="$DISCOBOX_BATS_TMP/state"
  export DISCOBOX_BATS_DB="${DISCOBOX_BATS_DB:-$DISCOBOX_BATS_TMP/discobox.sqlite}"
  export DISCOBOX_BATS_SERVER_LOG="${DISCOBOX_BATS_SERVER_LOG:-$DISCOBOX_BATS_TMP/server.log}"
  export DISCOBOX_BATS_POOL_FILE="$DISCOBOX_BATS_TMP/pool-id"
  export DISCOBOX_BATS_CONFIGURE_LOG="${DISCOBOX_BATS_CONFIGURE_LOG:-$DISCOBOX_BATS_TMP/configure.log}"
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
  go tool task build:harness-stub-image
  build_keep_stub_image

  PORT="$DISCOBOX_BATS_PORT" \
  DATABASE_DSN="$DISCOBOX_BATS_DB" \
  DISCOBOX_DATA_DIR="$DISCOBOX_BATS_DATA_DIR" \
  DISCOBOX_CONFIG_DIR="$DISCOBOX_BATS_CONFIG_DIR" \
  DISCOBOX_CACHE_DIR="$DISCOBOX_BATS_CACHE_DIR" \
  DISCOBOX_STATE_DIR="$DISCOBOX_BATS_STATE_DIR" \
  DISCOBOX_DOCKER_POOL_IMAGE=discobox-pool-agent:local \
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

# build_keep_stub_image derives a second stub image whose configure command keeps
# the previous secret instead of storing a new one. It has to be a separate image
# because STUB_CONFIGURE_KEEP can only be set at build time: a HarnessConfig
# carries no env, and `harness update` cannot swap an image.
#
# The toggle is declared in image.json's env, which is how a sandbox process gets
# its environment: the sandbox-agent reads image.json and applies its env when it
# starts the command (config.ApplyImageEnvDefaults). A Dockerfile ENV is not that
# path — it belongs to the container, whose PID 1 is systemd, and systemd does
# not pass its own environment to the services it starts.
#
# Only image.json changes; the harness metadata label lives outside the env block
# and is inherited. The two images register side by side because `harness create`
# takes an explicit --slug and --name, both unique per project.
build_keep_stub_image() {
  local ctx="$DISCOBOX_BATS_TMP/keep-stub"
  mkdir -p "$ctx"
  python3 - "$REPO_ROOT/test/harness-stub/image.json" "$ctx/image.json" <<'KEEPIMAGE'
import json
import sys

source, target = sys.argv[1:]
with open(source) as fh:
    image = json.load(fh)
image.setdefault("env", {})["STUB_CONFIGURE_KEEP"] = "1"
with open(target, "w") as fh:
    json.dump(image, fh)
KEEPIMAGE
  docker build -t discobox-harness-stub-keep:local -f - "$ctx" <<'DOCKERFILE'
# syntax=docker/dockerfile:1.7
FROM discobox-harness-stub:local
COPY image.json /usr/share/discobox/image.json
DOCKERFILE
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

# query runs one SQL statement against the server's database and prints the rows
# as tab-separated values, so assertions can reach state the API does not expose
# (secret IDs, bindings, grants).
#
# Secrets, bindings, and grants are soft-deleted (gorm.DeletedAt), so every
# statement here has to say "AND deleted_at IS NULL" itself. Without it a
# reconfigure looks like it leaked the secret it actually replaced.
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
  return 1
}

# ensure_pool waits for the pool the server seeds at startup and remembers its
# id for teardown. Nothing here picks a pool: a sandbox goes on the pool it names
# or on the project's default pool, and the configure flow names none, so the
# project default is where the configure sandbox lands. That makes the seeded
# default pool the one that has to be healthy, so this file uses it rather than
# creating a second provider. Its provider already binds the host Docker socket
# (so locally built harness images are visible) and reads PORT for its
# control-plane URL; DISCOBOX_DOCKER_POOL_IMAGE points it at the locally built
# pool-agent instead of an unpublished ghcr tag.
ensure_pool() {
  if [ -s "$DISCOBOX_BATS_POOL_FILE" ]; then
    cat "$DISCOBOX_BATS_POOL_FILE"
    return 0
  fi

  local pool_id=""
  for _ in {1..60}; do
    pool_id="$(query "SELECT id FROM pools LIMIT 1")"
    [ -n "$pool_id" ] && break
    sleep 1
  done
  [ -n "$pool_id" ] || return 1
  wait_for_pool_ready "$pool_id" || return 1
  printf '%s' "$pool_id" >"$DISCOBOX_BATS_POOL_FILE"
  cat "$DISCOBOX_BATS_POOL_FILE"
}

# configure_stub runs one full configure of a harness and prints what the
# configure terminal produced, which is where the stub echoes the seed it found.
#
# The output is unwrapped first: it comes back through a sandbox terminal, which
# hard-wraps at its column width, so the echoed JSON arrives split across lines
# and would not match as a single string.
configure_stub() {
  local harness="$1" raw status
  raw="$("$REPO_ROOT/build/disco" --server "$DISCOBOX_BATS_SERVER" --project default \
    box harness configure "$harness" </dev/null 2>&1)"
  status=$?
  printf '===== configure %s =====\n%s\n' "$harness" "$raw" >>"$DISCOBOX_BATS_CONFIGURE_LOG"
  raw="${raw//$'\r'/}"
  printf '%s' "${raw//$'\n'/}"
  return "$status"
}

@test "configure applies the stub's secret, binding, grant, and file" {
  local pool_id harness_json harness_id
  pool_id="$(ensure_pool)"
  [ -n "$pool_id" ]

  run cli box harness create --image discobox-harness-stub:local
  [ "$status" -eq 0 ]
  harness_json="$output"
  harness_id="$(printf '%s' "$harness_json" | json_get id)"
  [ -n "$harness_id" ]
  [ "$(printf '%s' "$harness_json" | json_get configured)" = "False" ]

  run configure_stub stub
  [ "$status" -eq 0 ]
  # Nothing was configured before, so there is no seed to echo.
  [[ "$output" == *"stub configure: PREV_STUB_TOKEN is unset"* ]]
  [[ "$output" == *"stub configure: done"* ]]

  run cli box harness get stub
  [ "$status" -eq 0 ]
  [ "$(printf '%s' "$output" | json_get configured)" = "True" ]

  # The secret, its binding, and the harnessConfig-scoped grant that makes it
  # usable at run time. A binding alone is not a grant.
  run query "SELECT COUNT(*) FROM secrets WHERE name = 'stub-token' AND deleted_at IS NULL"
  [ "$output" = "1" ]
  run query "SELECT env_name FROM harness_config_secret_bindings WHERE harness_config_id = '$harness_id' AND deleted_at IS NULL"
  [ "$output" = "STUB_TOKEN" ]
  run query "SELECT COUNT(*) FROM secret_grants g JOIN secrets s ON s.id = g.secret_id WHERE s.name = 'stub-token' AND g.deleted_at IS NULL AND s.deleted_at IS NULL"
  [ "$output" = "1" ]
}

@test "reconfigure seeds the previous config without values and offers PREV_" {
  local before_id after_id
  before_id="$(query "SELECT id FROM secrets WHERE name = 'stub-token' AND deleted_at IS NULL")"
  [ -n "$before_id" ]

  run configure_stub stub
  [ "$status" -eq 0 ]

  # The seed landed before the command ran, and describes the first run's result.
  [[ "$output" == *'"envName":"STUB_TOKEN"'* ]]
  [[ "$output" == *'"usePrevious":true'* ]]
  [[ "$output" == *'"path":"stub.json"'* ]]
  # The whole point: metadata, never a value. The first run stored s3cr3t, and
  # the seed must not carry it in any form.
  [[ "$output" != *"s3cr3t"* ]]
  [[ "$output" != *'"value"'* ]]
  # The value is offered separately, as a sentinel under the PREV_ prefix.
  [[ "$output" == *"stub configure: PREV_STUB_TOKEN is set"* ]]

  # This run returned a fresh value, so it replaces the previous generation
  # rather than leaking an orphan alongside it.
  run query "SELECT COUNT(*) FROM secrets WHERE name = 'stub-token' AND deleted_at IS NULL"
  [ "$output" = "1" ]
  after_id="$(query "SELECT id FROM secrets WHERE name = 'stub-token' AND deleted_at IS NULL")"
  [ "$after_id" != "$before_id" ]
}

@test "a configure that returns usePrevious keeps the existing secret" {
  local pool_id harness_id before_id after_id before_grant after_grant
  pool_id="$(ensure_pool)"

  run cli box harness create --image discobox-harness-stub-keep:local --slug stub-keep --name "Stub (keep)"
  [ "$status" -eq 0 ]
  harness_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$harness_id" ]

  # First run has nothing to keep, so it stores a value like any other flow.
  run configure_stub stub-keep
  [ "$status" -eq 0 ]
  [[ "$output" == *"stub configure: PREV_STUB_TOKEN is unset"* ]]
  before_id="$(query "SELECT s.id FROM secrets s JOIN harness_config_secret_bindings b ON b.secret_id = s.id WHERE b.harness_config_id = '$harness_id' AND b.deleted_at IS NULL AND s.deleted_at IS NULL")"
  [ -n "$before_id" ]
  before_grant="$(query "SELECT id FROM secret_grants WHERE secret_id = '$before_id' AND deleted_at IS NULL")"
  [ -n "$before_grant" ]

  # Second run returns usePrevious and no value at all.
  run configure_stub stub-keep
  [ "$status" -eq 0 ]
  [[ "$output" == *"stub configure: PREV_STUB_TOKEN is set"* ]]

  run cli box harness get stub-keep
  [ "$status" -eq 0 ]
  [ "$(printf '%s' "$output" | json_get configured)" = "True" ]

  # The same secret row survives, with its binding and grant intact — a kept
  # secret is carried over, not recreated, and not swept as a stale generation.
  after_id="$(query "SELECT s.id FROM secrets s JOIN harness_config_secret_bindings b ON b.secret_id = s.id WHERE b.harness_config_id = '$harness_id' AND b.deleted_at IS NULL AND s.deleted_at IS NULL")"
  [ "$after_id" = "$before_id" ]
  after_grant="$(query "SELECT id FROM secret_grants WHERE secret_id = '$before_id' AND deleted_at IS NULL")"
  [ "$after_grant" = "$before_grant" ]
  run query "SELECT COUNT(*) FROM secrets WHERE id = '$before_id' AND deleted_at IS NULL"
  [ "$output" = "1" ]
}

@test "deconfigure removes exactly what configure created" {
  local harness_id
  harness_id="$(cli box harness get stub | json_get id)"
  [ -n "$harness_id" ]

  run cli box harness deconfigure stub
  [ "$status" -eq 0 ]
  [ "$(printf '%s' "$output" | json_get configured)" = "False" ]

  run query "SELECT COUNT(*) FROM harness_config_secret_bindings WHERE harness_config_id = '$harness_id' AND deleted_at IS NULL"
  [ "$output" = "0" ]
  # The stub-keep harness still holds its own, so this counts only the one the
  # deconfigured harness created.
  run query "SELECT COUNT(*) FROM secrets s JOIN harness_config_secret_bindings b ON b.secret_id = s.id WHERE b.harness_config_id = '$harness_id' AND b.deleted_at IS NULL AND s.deleted_at IS NULL"
  [ "$output" = "0" ]
}
