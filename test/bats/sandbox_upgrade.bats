#!/usr/bin/env bats
#
# End-to-end coverage of sandbox image upgrades (ADR 0016).
#
# What is unique to this file is the whole point of the feature, and it cannot be
# reached by a unit test: a harness image REBUILT UNDER THE SAME TAG must be
# noticed, offered as an upgrade, and applied by replacing the sandbox's
# container while the sandbox keeps its identity and its pool-host state.
#
# This is the failure that motivated the ADR. A sandbox was created from a
# harness image, the image moved on, and nothing in the system could say so —
# the skew only surfaced four layers down as a 500 from a terminal attach.
#
# The stub is built twice under one tag, so only the config digest distinguishes
# the two builds. Comparing image references would pass this file while missing
# every real rebuild, which is exactly the bug being tested for.
#
# The pool binds the host Docker socket so locally built images are visible
# without a registry.

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"

  export DISCOBOX_BATS_TMP="$BATS_SUITE_TMPDIR/discobox-sandbox-upgrade"
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
  # Name both listen endpoints explicitly. The server opens no TCP listener
  # unless DISCOBOX_SERVER_LISTEN asks for one, and without a unix endpoint of
  # its own it falls back to the machine's default IPC socket — which it then
  # RECLAIMS, shutting down the developer's running server. A private socket in
  # this suite's temp dir keeps the run isolated.
  export DISCOBOX_BATS_SOCKET="$DISCOBOX_BATS_TMP/server.sock"

  (cd server && go build -o ../build/discobox-server ./cmd/discobox-server)
  rm -f build/disco
  (cd cli && go build -o ../build/disco ./cmd/disco)
  (docker build -f pool-agent/Dockerfile -t discobox-pool-agent:local .)
  go tool task build:harness-stub-image

  PORT="$DISCOBOX_BATS_PORT" \
  DISCOBOX_SERVER_LISTEN="unix://$DISCOBOX_BATS_SOCKET,http://127.0.0.1:$DISCOBOX_BATS_PORT" \
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

export UPGRADE_STUB_IMAGE="discobox-harness-upgrade-stub:local"

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
        SELECT id, ready, schedulable, state, generation, observed_generation
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

# configure_stub runs one full configure of a harness. A harness is only
# selectable for a sandbox once its configure flow has succeeded, so this is a
# precondition for having a harness-backed (and therefore image-pinned) sandbox
# at all, not something this file is testing.
configure_stub() {
  local harness="$1" raw status
  raw="$("$REPO_ROOT/build/disco" --server "$DISCOBOX_BATS_SERVER" --project default \
    box harness configure "$harness" </dev/null 2>&1)"
  status=$?
  printf '===== configure %s =====\n%s\n' "$harness" "$raw" >>"$DISCOBOX_BATS_CONFIGURE_LOG"
  return "$status"
}

# build_upgrade_stub builds the stub harness under this file's own tag, with a
# marker baked into the label so a second call produces different image content
# under the SAME reference. A dedicated tag keeps this file from clobbering
# discobox-harness-stub:local, which other suites share.
build_upgrade_stub() {
  local marker="$1" ctx="$DISCOBOX_BATS_TMP/upgrade-stub-$marker"
  mkdir -p "$ctx"
  cp "$REPO_ROOT/test/harness-stub/Dockerfile" "$REPO_ROOT/test/harness-stub/configure.sh" "$ctx/"
  python3 - "$REPO_ROOT/test/harness-stub/image.json" "$ctx/image.json" "$marker" <<'STUBIMAGE'
import json
import sys

source, target, marker = sys.argv[1:]
with open(source) as handle:
    image = json.load(handle)
env = image.setdefault("env", {})
env["STUB_BUILD_MARKER"] = marker
with open(target, "w") as handle:
    json.dump(image, handle)
STUBIMAGE
  docker build -f "$ctx/Dockerfile" \
    --build-arg SANDBOX_AGENT_IMAGE="$(grep -s '^DISCOBOX_DEFAULT_SANDBOX_IMAGE=' "$REPO_ROOT/.env" | cut -d= -f2- | grep . || echo discobox-sandbox-agent:local)" \
    --build-arg HARNESS_METADATA="$(jq -c . "$ctx/image.json")" \
    -t "$UPGRADE_STUB_IMAGE" "$ctx" >>"${DISCOBOX_BATS_STUB_BUILD_LOG:-$DISCOBOX_BATS_TMP/stub-build.log}" 2>&1
}

# sandbox_container_image prints the image ID a sandbox's container was actually
# built from. This is the ground truth the whole feature is about: the control
# plane can claim whatever it likes, but the container either runs the pinned
# image or it does not.
sandbox_container_image() {
  local sandbox_id="$1" container
  container="$(docker ps -aq --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)"
  [ -n "$container" ] || return 1
  docker inspect "$container" --format '{{.Image}}'
}

sandbox_field() {
  "$REPO_ROOT/build/disco" --server "$DISCOBOX_BATS_SERVER" --project default --output json \
    box sandbox get "$1" | json_get "$2"
}

wait_for_sandbox_state() {
  local sandbox_id="$1" want="$2"
  for _ in {1..90}; do
    case "$(sandbox_field "$sandbox_id" runtime.displayState)" in
      "$want") return 0 ;;
      error) break ;;
    esac
    sleep 1
  done
  echo "sandbox did not reach $want: $sandbox_id" >&2
  "$REPO_ROOT/build/disco" --server "$DISCOBOX_BATS_SERVER" --project default --output json \
    box sandbox get "$sandbox_id" >&2 || true
  tail -100 "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
  return 1
}

wait_for_sandbox_running() {
  local sandbox_id="$1"
  for _ in {1..90}; do
    case "$(sandbox_field "$sandbox_id" runtime.displayState)" in
      running) return 0 ;;
      error) break ;;
    esac
    sleep 1
  done
  echo "sandbox did not return to running: $sandbox_id" >&2
  "$REPO_ROOT/build/disco" --server "$DISCOBOX_BATS_SERVER" --project default --output json \
    box sandbox get "$sandbox_id" >&2 || true
  tail -100 "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
  return 1
}

@test "a harness image rebuilt under the same tag is offered and applied as an upgrade" {
  local pool_id harness_id sandbox_id before_image after_image before_digest after_digest before_mounts after_mounts
  pool_id="$(ensure_pool)"
  [ -n "$pool_id" ]

  build_upgrade_stub v1

  run cli box harness create --image "$UPGRADE_STUB_IMAGE" --slug upgrade-stub --name "Upgrade Stub"
  [ "$status" -eq 0 ]
  harness_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$harness_id" ]
  before_digest="$(printf '%s' "$output" | json_get imageDigest)"
  [ -n "$before_digest" ]

  run configure_stub upgrade-stub
  [ "$status" -eq 0 ]

  run cli box sandbox create --name "upgrade-e2e" --harness upgrade-stub --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  sandbox_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$sandbox_id" ]

  # The sandbox pins the digest it was built from, and reports nothing to do.
  [ "$(sandbox_field "$sandbox_id" config.imageDigest)" = "$before_digest" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.available)" != "True" ]

  before_image="$(sandbox_container_image "$sandbox_id")"
  [ -n "$before_image" ]
  before_mounts="$(docker inspect "$(docker ps -aq --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)" \
    --format '{{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}')"

  # Rebuild the SAME tag. Nothing about the reference changes; only the content.
  build_upgrade_stub v2
  run cli box harness refresh-image upgrade-stub
  [ "$status" -eq 0 ]
  after_digest="$(printf '%s' "$output" | json_get imageDigest)"
  [ -n "$after_digest" ]
  [ "$after_digest" != "$before_digest" ]

  # The sandbox now reports the upgrade. A tag comparison would report nothing.
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.available)" = "True" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.reason)" = "imageDigestChanged" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.targetImageDigest)" = "$after_digest" ]

  # ... but has not moved on its own. A running sandbox never changes image
  # without being asked.
  [ "$(sandbox_container_image "$sandbox_id")" = "$before_image" ]

  run cli box sandbox upgrade "$sandbox_id"
  [ "$status" -eq 0 ]
  wait_for_sandbox_running "$sandbox_id"

  # Same sandbox, new container, new image, re-pinned.
  after_image="$(sandbox_container_image "$sandbox_id")"
  [ -n "$after_image" ]
  [ "$after_image" != "$before_image" ]
  [ "$(sandbox_field "$sandbox_id" config.imageDigest)" = "$after_digest" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.available)" != "True" ]

  # The state the sandbox actually keeps — its pool-host binds — crosses the
  # replacement untouched. This is what makes an in-place upgrade honest.
  after_mounts="$(docker inspect "$(docker ps -aq --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)" \
    --format '{{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}')"
  [ "$after_mounts" = "$before_mounts" ]

  # A second upgrade has nothing to do and is refused rather than performed:
  # recreating a container costs container-local state for no gain.
  run cli box sandbox upgrade "$sandbox_id"
  [ "$status" -ne 0 ]
}

# Upgrading is not a way to start a sandbox (ADR 0021 §3). A stopped sandbox gets
# the new image and stays stopped; a running one comes back up on it. Only the
# pool agent can know which case it is in, so this is the assertion that the
# control plane never decides power state.
@test "an upgrade preserves the power state of the sandbox it replaces" {
  local pool_id harness_id sandbox_id before_image after_image after_digest
  pool_id="$(ensure_pool)"
  [ -n "$pool_id" ]

  build_upgrade_stub power-v1

  run cli box harness create --image "$UPGRADE_STUB_IMAGE" --slug power-stub --name "Power Stub"
  [ "$status" -eq 0 ]
  harness_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$harness_id" ]

  run configure_stub power-stub
  [ "$status" -eq 0 ]

  run cli box sandbox create --name "upgrade-power-e2e" --harness power-stub --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  sandbox_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$sandbox_id" ]
  before_image="$(sandbox_container_image "$sandbox_id")"
  [ -n "$before_image" ]

  run cli box sandbox stop "$sandbox_id"
  [ "$status" -eq 0 ]
  wait_for_sandbox_state "$sandbox_id" stopped

  build_upgrade_stub power-v2
  run cli box harness refresh-image power-stub
  [ "$status" -eq 0 ]
  after_digest="$(printf '%s' "$output" | json_get imageDigest)"
  [ -n "$after_digest" ]

  run cli box sandbox upgrade "$sandbox_id"
  [ "$status" -eq 0 ]

  # The container is rebuilt on the new image without being started: an upgrade
  # delivers an image, never a power-on.
  for _ in {1..90}; do
    after_image="$(sandbox_container_image "$sandbox_id" || true)"
    [ -n "$after_image" ] && [ "$after_image" != "$before_image" ] && break
    sleep 1
  done
  [ -n "$after_image" ]
  [ "$after_image" != "$before_image" ]
  [ "$(sandbox_field "$sandbox_id" config.imageDigest)" = "$after_digest" ]
  wait_for_sandbox_state "$sandbox_id" stopped

  # And a start after the upgrade is an ordinary start of the rebuilt container.
  run cli box sandbox start "$sandbox_id"
  [ "$status" -eq 0 ]
  wait_for_sandbox_running "$sandbox_id"
  [ "$(sandbox_container_image "$sandbox_id")" = "$after_image" ]
}
