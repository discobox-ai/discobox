#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats runs tests and hooks under set -e, so a failed cd already fails.
#
# End-to-end coverage of sandbox upgrades (ADR 0016).
#
# What is unique to this file is the whole point of the feature, and it cannot be
# reached by a unit test: a harness bundle REBUILT UNDER THE SAME TAG must be
# noticed, offered as an upgrade, and applied by replacing the sandbox's
# container while the sandbox keeps its identity and its pool-host state.
#
# This is the failure that motivated the ADR. A sandbox was created from a
# harness, the harness moved on, and nothing in the system could say so — the
# skew only surfaced four layers down as a 500 from a terminal attach.
#
# The stub bundle is built twice under one tag, so only its digest
# distinguishes the two builds. Comparing references would pass this file while
# missing every real rebuild, which is exactly the bug being tested for.
#
# No harness is a runnable image: every sandbox container is the Docker
# driver's sandbox base image (discobox-sandbox-base:local, built here and
# named to the server so boxes do not pull the remote default), with the
# platform and harness bundles applied by the sandbox agent. So an upgrade is
# observed as a new container, not a new image.

setup_file() {
  REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  export REPO_ROOT
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

  DISCOBOX_BATS_PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
  export DISCOBOX_BATS_PORT
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:$DISCOBOX_BATS_PORT"
  # Name both listen endpoints explicitly. The server opens no TCP listener
  # unless DISCOBOX_SERVER_LISTEN asks for one, and without a unix endpoint of
  # its own it falls back to the machine's default IPC socket — which it then
  # RECLAIMS, shutting down the developer's running server. A private socket in
  # this suite's temp dir keeps the run isolated.
  export DISCOBOX_BATS_SOCKET="$DISCOBOX_BATS_TMP/server.sock"

  (cd server && go build -o ../build/discobox-server ./cmd/discobox-server)
  rm -f build/discobox
  (cd cli && go build -o ../build/discobox ./cmd/discobox)
  # The sandbox base image is every sandbox container's, and every Docker
  # pool's, which is provisioned from the pool bundle and the agent bundle
  # (ADR 26-10-24-953 §2). The platform
  # bundle is what the server composes every harness bundle on, and what the
  # agent applies first in every box.
  #
  # Every bundle must be servable to the pool, which fetches them through the
  # server's asset API; that API reads OCI layouts or registries, not the
  # daemon's :local tags.
  go tool task build:sandbox-base-image
  go tool task build:pool-agent-bundle
  go tool task build:agent-bundle
  go tool task build:platform-bundle
  go tool task build:harness-stub-bundle

  PORT="$DISCOBOX_BATS_PORT" \
  DISCOBOX_SERVER_LISTEN="unix://$DISCOBOX_BATS_SOCKET,http://127.0.0.1:$DISCOBOX_BATS_PORT" \
  DATABASE_DSN="$DISCOBOX_BATS_DB" \
  DISCOBOX_DATA_DIR="$DISCOBOX_BATS_DATA_DIR" \
  DISCOBOX_CONFIG_DIR="$DISCOBOX_BATS_CONFIG_DIR" \
  DISCOBOX_CACHE_DIR="$DISCOBOX_BATS_CACHE_DIR" \
  DISCOBOX_STATE_DIR="$DISCOBOX_BATS_STATE_DIR" \
  DISCOBOX_POOL_AGENT_BUNDLE=discobox-pool-agent-bundle:local \
  DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE=discobox-sandbox-base:local \
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
    cli admin pool rm "$pool_id" >/dev/null 2>&1 || true
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
  # (discobox-sandbox-base:local can share an image ID with a developer's own dev
  # tag, so it would reap their running pools).
  for pool_id in $pool_ids; do
    docker ps -aq --filter "label=discobox.pool_id=$pool_id" | xargs -r docker rm -f >/dev/null 2>&1 || true
    docker network rm "discobox-sbnet-$pool_id" >/dev/null 2>&1 || true
  done
}

export UPGRADE_STUB_BUNDLE="discobox-harness-upgrade-stub:local"

cli() {
  "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default --output json "$@"
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
# (so the locally built sandbox base image is visible) and reads PORT for its
# control-plane URL; DISCOBOX_POOL_AGENT_BUNDLE points it at the locally built
# pool bundle instead of an unpublished ghcr tag.
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
# precondition for having a harness-backed (and therefore bundle-pinned) sandbox
# at all, not something this file is testing.
configure_stub() {
  local harness="$1" raw status
  raw="$("$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default \
    admin harness configure "$harness" </dev/null 2>&1)"
  status=$?
  printf '===== configure %s =====\n%s\n' "$harness" "$raw" >>"$DISCOBOX_BATS_CONFIGURE_LOG"
  return "$status"
}

# build_upgrade_stub builds the stub's bundle under this file's own tag, with a
# marker baked into its manifest label so a second call produces different
# content under the SAME reference: an env in the label, which moves the
# bundle's digest, the one a sandbox pins. It is derived from the shared stub
# bundle, which setup_file builds, rather than rebuilt from its Dockerfile. A
# dedicated tag keeps this file from clobbering discobox-harness-stub:local,
# which other suites share.
build_upgrade_stub() {
  local marker="$1" metadata log="${DISCOBOX_BATS_STUB_BUILD_LOG:-$DISCOBOX_BATS_TMP/stub-build.log}"
  metadata="$(jq -c --arg marker "$marker" '.env.STUB_BUILD_MARKER = $marker' "$REPO_ROOT/test/harness-stub/image.json")"
  docker build --build-arg HARNESS_METADATA="$metadata" -t "$UPGRADE_STUB_BUNDLE" - >>"$log" 2>&1 <<'DOCKERFILE'
FROM discobox-harness-stub:local
ARG HARNESS_METADATA
LABEL io.discobox.image.v1="${HARNESS_METADATA:?HARNESS_METADATA build argument is required}"
DOCKERFILE
}

# sandbox_container_id prints the full ID of a sandbox's container. Every
# container is the same sandbox base image, so the image says nothing about an
# upgrade; a new container does. This is the ground truth the whole feature is
# about: the control plane can claim whatever it likes, but the container is
# either replaced or it is not.
sandbox_container_id() {
  local sandbox_id="$1" container
  container="$(docker ps -aq --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)"
  [ -n "$container" ] || return 1
  docker inspect "$container" --format '{{.Id}}'
}

sandbox_field() {
  "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default --output json \
    admin box get "$1" | json_get "$2"
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
  "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default --output json \
    admin box get "$sandbox_id" >&2 || true
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
  "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default --output json \
    admin box get "$sandbox_id" >&2 || true
  tail -100 "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
  return 1
}

@test "a harness bundle rebuilt under the same tag is offered and applied as an upgrade" {
  local pool_id harness_id sandbox_id before_container after_container before_digest after_digest before_mounts after_mounts
  pool_id="$(ensure_pool)"
  [ -n "$pool_id" ]

  build_upgrade_stub v1

  run cli admin harness create --bundle "$UPGRADE_STUB_BUNDLE" --slug upgrade-stub --name "Upgrade Stub"
  [ "$status" -eq 0 ]
  harness_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$harness_id" ]
  before_digest="$(printf '%s' "$output" | json_get bundleDigest)"
  [ -n "$before_digest" ]

  run configure_stub upgrade-stub
  [ "$status" -eq 0 ]

  run cli admin box create --name "upgrade-e2e" --harness upgrade-stub --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  sandbox_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$sandbox_id" ]

  # The sandbox pins the digest it was built from, and reports nothing to do.
  [ "$(sandbox_field "$sandbox_id" config.bundleDigest)" = "$before_digest" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.available)" != "True" ]

  before_container="$(sandbox_container_id "$sandbox_id")"
  [ -n "$before_container" ]
  before_mounts="$(docker inspect "$(docker ps -aq --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)" \
    --format '{{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}')"

  # Rebuild the SAME tag. Nothing about the reference changes; only the content.
  build_upgrade_stub v2
  run cli admin harness refresh upgrade-stub
  [ "$status" -eq 0 ]
  after_digest="$(printf '%s' "$output" | json_get bundleDigest)"
  [ -n "$after_digest" ]
  [ "$after_digest" != "$before_digest" ]

  # The sandbox now reports the upgrade. A tag comparison would report nothing.
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.available)" = "True" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.reason)" = "bundleDigestChanged" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.targetBundleDigest)" = "$after_digest" ]

  # ... but has not moved on its own. A running sandbox never changes bundle
  # without being asked.
  [ "$(sandbox_container_id "$sandbox_id")" = "$before_container" ]

  run cli admin box upgrade "$sandbox_id"
  [ "$status" -eq 0 ]

  # The upgrade only re-pins; the pool replaces the container afterwards. Until
  # it does, the old container still reads as running, so waiting for running
  # alone returns before anything has been replaced.
  for _ in {1..90}; do
    after_container="$(sandbox_container_id "$sandbox_id" || true)"
    [ -n "$after_container" ] && [ "$after_container" != "$before_container" ] && break
    sleep 1
  done
  wait_for_sandbox_running "$sandbox_id"

  # Same sandbox, a new container (container ID changed), re-pinned to the new
  # bundle.
  after_container="$(sandbox_container_id "$sandbox_id")"
  [ -n "$after_container" ]
  [ "$after_container" != "$before_container" ]
  [ "$(sandbox_field "$sandbox_id" config.bundleDigest)" = "$after_digest" ]
  [ "$(sandbox_field "$sandbox_id" runtime.upgrade.available)" != "True" ]

  # The state the sandbox actually keeps — its pool-host binds — crosses the
  # replacement untouched. This is what makes an in-place upgrade honest.
  after_mounts="$(docker inspect "$(docker ps -aq --filter "label=discobox.sandbox_id=$sandbox_id" | head -1)" \
    --format '{{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}')"
  [ "$after_mounts" = "$before_mounts" ]

  # A second upgrade has nothing to do and is refused rather than performed:
  # recreating a container costs container-local state for no gain.
  run cli admin box upgrade "$sandbox_id"
  [ "$status" -ne 0 ]
}

# Upgrading is not a way to start a sandbox (ADR 0021 §3). A stopped sandbox gets
# the new bundle and stays stopped; a running one comes back up on it. Only the
# pool agent can know which case it is in, so this is the assertion that the
# control plane never decides power state.
#
# The project is held to manual upgrades: under the default automatic policy
# (ADR 0082) the harness refresh alone moves a stopped sandbox, and the explicit
# upgrade this test drives is then refused as having nothing to do.
@test "an upgrade preserves the power state of the sandbox it replaces" {
  local pool_id harness_id sandbox_id before_container after_container after_digest
  pool_id="$(ensure_pool)"
  [ -n "$pool_id" ]

  run cli admin project update default --sandbox-upgrade-policy manual
  [ "$status" -eq 0 ]

  build_upgrade_stub power-v1

  run cli admin harness create --bundle "$UPGRADE_STUB_BUNDLE" --slug power-stub --name "Power Stub"
  [ "$status" -eq 0 ]
  harness_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$harness_id" ]

  run configure_stub power-stub
  [ "$status" -eq 0 ]

  run cli admin box create --name "upgrade-power-e2e" --harness power-stub --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  sandbox_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$sandbox_id" ]
  before_container="$(sandbox_container_id "$sandbox_id")"
  [ -n "$before_container" ]

  run cli admin box stop "$sandbox_id"
  [ "$status" -eq 0 ]
  wait_for_sandbox_state "$sandbox_id" stopped

  build_upgrade_stub power-v2
  run cli admin harness refresh power-stub
  [ "$status" -eq 0 ]
  after_digest="$(printf '%s' "$output" | json_get bundleDigest)"
  [ -n "$after_digest" ]

  run cli admin box upgrade "$sandbox_id"
  [ "$status" -eq 0 ]

  # The container is rebuilt on the new bundle without being started: an
  # upgrade delivers a bundle, never a power-on.
  for _ in {1..90}; do
    after_container="$(sandbox_container_id "$sandbox_id" || true)"
    [ -n "$after_container" ] && [ "$after_container" != "$before_container" ] && break
    sleep 1
  done
  [ -n "$after_container" ]
  [ "$after_container" != "$before_container" ]
  [ "$(sandbox_field "$sandbox_id" config.bundleDigest)" = "$after_digest" ]
  wait_for_sandbox_state "$sandbox_id" stopped

  # And a start after the upgrade is an ordinary start of the rebuilt container.
  run cli admin box start "$sandbox_id"
  [ "$status" -eq 0 ]
  wait_for_sandbox_running "$sandbox_id"
  [ "$(sandbox_container_id "$sandbox_id")" = "$after_container" ]
}
