#!/usr/bin/env bats
#
# End-to-end coverage of moving a discobox between servers (ADR 0123).
#
# What is unique to this file cannot be reached by a unit test, and is the whole
# claim the feature makes: bytes written INSIDE a running sandbox come back
# inside a different sandbox, after a real tar has crossed a real pool agent in
# both directions. Every layer below is stubbed somewhere in the Go tests --
# the tar walk against a temp dir, the manifest against a fake provider, the
# ordering against a recording one -- and none of them can say whether a
# workspace actually survives the trip.
#
# The pool binds the host Docker socket so locally built images are visible
# without a registry.

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"

  export DISCOBOX_BATS_TMP="$BATS_SUITE_TMPDIR/discobox-sandbox-transfer"
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
  rm -f build/discobox
  (cd cli && go build -o ../build/discobox ./cmd/discobox)
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

export TRANSFER_STUB_IMAGE="discobox-harness-transfer-stub:local"

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
  raw="$("$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" --project default \
    admin harness configure "$harness" </dev/null 2>&1)"
  status=$?
  printf '===== configure %s =====\n%s\n' "$harness" "$raw" >>"$DISCOBOX_BATS_CONFIGURE_LOG"
  return "$status"
}

# build_transfer_stub builds the stub harness under this file's own tag, with a
# marker baked into the label so a second call produces different image content
# under the SAME reference. A dedicated tag keeps this file from clobbering
# discobox-harness-stub:local, which other suites share.
build_transfer_stub() {
  local marker="$1" ctx="$DISCOBOX_BATS_TMP/transfer-stub-$marker"
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
    -t "$TRANSFER_STUB_IMAGE" "$ctx" >>"${DISCOBOX_BATS_STUB_BUILD_LOG:-$DISCOBOX_BATS_TMP/stub-build.log}" 2>&1
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


# sandbox_container is the container id a sandbox is actually running, which is
# where a test writes and reads the bytes the feature is about.
sandbox_container() {
  docker ps -aq --filter "label=discobox.sandbox_id=$1" | head -1
}

# sandbox_home is the sandbox user's home as the container sees it: the mount
# point of the `data` subtree, which is the half of the tree an export carries.
sandbox_home() {
  docker exec "$(sandbox_container "$1")" sh -lc 'printf %s "$HOME"'
}

@test "a discobox's workspace survives an export and an import" {
  local pool_id source_id restored_id home archive first_member extracted truncated
  pool_id="$(ensure_pool)"
  [ -n "$pool_id" ]

  build_transfer_stub v1
  run cli admin harness create --image "$TRANSFER_STUB_IMAGE" --slug transfer-stub --name "Transfer Stub"
  [ "$status" -eq 0 ]
  run configure_stub transfer-stub
  [ "$status" -eq 0 ]

  run cli admin box create --name "transfer-source" --harness transfer-stub --wait --wait-timeout 120s
  [ "$status" -eq 0 ]
  source_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$source_id" ]
  wait_for_sandbox_running "$source_id"

  # Written from inside the sandbox, as the sandbox user, into the home the
  # harness works in. Nothing in the control plane or the pool agent put it
  # there, which is what makes it a real assertion.
  home="$(sandbox_home "$source_id")"
  [ -n "$home" ]
  docker exec "$(sandbox_container "$source_id")" sh -lc \
    "mkdir -p '$home/work/nested' && printf 'the-payload-42\n' > '$home/work/nested/keepsake.txt'"
  # And state the image declares stays behind (ADR 0129 §2): the nested Docker
  # daemon's, written where the daemon keeps it, as root, the way it would be.
  docker exec "$(sandbox_container "$source_id")" sh -c \
    "printf 'rebuildable\n' > /var/lib/docker/transfer-marker"

  # A running discobox is refused, and the refusal says how to get past it.
  archive="$DISCOBOX_BATS_TMP/transfer-source.dbox"
  run cli admin box export "$source_id" -o "$archive"
  [ "$status" -ne 0 ]
  [[ "$output" == *"--stop"* ]]
  [ ! -e "$archive" ]

  run cli admin box export "$source_id" --stop -o "$archive"
  [ "$status" -eq 0 ]
  [ -s "$archive" ]

  # It is a tar, the manifest comes first so a reader knows what it holds before
  # the workspace arrives, and the tree rides under tree/.
  first_member="$(tar tf "$archive" | head -1)"
  [ "$first_member" = "manifest.json" ]
  tar tf "$archive" | grep -q "^tree/data/"
  # The export carries the discobox's work and not what was installed into it:
  # home and the agent's own state travel, the nested Docker store does not. The
  # sandbox agent read the tree, in the sandbox's own image (ADR 0129 §1).
  tar tf "$archive" | grep -q "/work/nested/keepsake.txt$"
  tar tf "$archive" | grep -q "^tree/data/var/lib/discobox/"
  # A test, not a negated command: bats does not fail on `! cmd`.
  [ -z "$(tar tf "$archive" | grep "^tree/data/var/lib/docker/")" ]
  tar xOf "$archive" manifest.json | python3 -c 'import json,sys; m=json.load(sys.stdin); assert m["formatVersion"]==1, m; assert m["sandbox"]["harness"]["slug"]=="transfer-stub", m'

  # No secret value is in the file, whatever else is.
  ! tar xOf "$archive" manifest.json | grep -qi '"token"\|BEGIN .*PRIVATE KEY'

  # It ends with a SHA256SUMS that stock tools check: extracted, `sha256sum -c`
  # verifies every file in it with nothing of ours installed.
  [ "$(tar tf "$archive" | tail -1)" = "SHA256SUMS" ]
  extracted="$DISCOBOX_BATS_TMP/transfer-source.extracted"
  rm -rf "$extracted"
  mkdir -p "$extracted"
  tar xf "$archive" -C "$extracted" --no-same-owner
  # The home holds files only their owner may read, and the owner is now us.
  chmod -R u+rwX "$extracted"
  (cd "$extracted" && sha256sum --quiet --strict -c SHA256SUMS)
  rm -rf "$extracted"

  # Without its SHA256SUMS -- what a stream cut between two files leaves, and a
  # plain tar reader accepts -- the archive is refused rather than restored as a
  # smaller workspace.
  truncated="$DISCOBOX_BATS_TMP/transfer-truncated.dbox"
  cp "$archive" "$truncated"
  tar --delete -f "$truncated" SHA256SUMS
  run cli admin box import "$truncated" --name "transfer-truncated"
  [ "$status" -ne 0 ]
  [[ "$output" == *"SHA256SUMS"* ]]

  run cli admin box import "$archive" --name "transfer-restored"
  if [ "$status" -ne 0 ]; then
    echo "import failed: $output" >&2
    tail -60 "$DISCOBOX_BATS_SERVER_LOG" >&2 || true
  fi
  [ "$status" -eq 0 ]
  restored_id="$(printf '%s' "$output" | json_get id)"
  [ -n "$restored_id" ]
  [ "$restored_id" != "$source_id" ]
  wait_for_sandbox_running "$restored_id"

  # The whole claim: the bytes written inside the first sandbox are inside the
  # second one, at the same path, with the same contents.
  run docker exec "$(sandbox_container "$restored_id")" sh -lc \
    "cat '$(sandbox_home "$restored_id")/work/nested/keepsake.txt'"
  [ "$status" -eq 0 ]
  [ "$output" = "the-payload-42" ]

  # And it is a different discobox, with its own container, not the old one
  # renamed.
  [ "$(sandbox_container "$restored_id")" != "$(sandbox_container "$source_id")" ]
}
