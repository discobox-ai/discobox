#!/usr/bin/env bats
#
# Exercises the Docker provider through the CLI against a development stack that
# is already running (`task dev`).
#
# This suite deliberately owns nothing but the resources it creates. It does not
# start a server, and it does not build images: a server started here would take
# the shared Unix socket away from the running one — a starting server asks the
# incumbent to shut down — and rebuilding images would swap them under whatever
# is already using them. It skips instead of failing when the stack is not up,
# so a run without `task dev` is a no-op rather than a wrong answer.
#
# Cleanup is by recorded ID, never by label or image filter: matching containers
# broadly would reap pools this suite did not create.

setup_file() {
  export REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  cd "$REPO_ROOT"

  command -v docker >/dev/null 2>&1 || skip "docker is required"
  docker info >/dev/null 2>&1 || skip "docker daemon is required"

  # The image watcher in `task dev` owns this tag.
  docker image inspect discobox-pool-agent:local >/dev/null 2>&1 ||
    skip "discobox-pool-agent:local is missing; run 'task dev' to build it"

  # The pool container reaches the control plane over the host network, so the
  # development server's HTTP port has to be up and reachable.
  export DISCOBOX_BATS_PORT="${PORT:-18080}"
  curl -fsS "http://127.0.0.1:$DISCOBOX_BATS_PORT/openapi.yaml" >/dev/null 2>&1 ||
    skip "no development server on port $DISCOBOX_BATS_PORT; run 'task dev'"

  # `task dev` hot-reloads the server, not the CLI; building it touches no
  # shared state.
  (cd cli && go build -o ../build/disco ./cmd/disco) || skip "cannot build the CLI"

  "$REPO_ROOT/build/disco" --output json box pool ls >/dev/null 2>&1 ||
    skip "development server is not answering API requests"

  # Everything this suite creates carries this suffix, so anything a crash
  # leaves behind is identifiable and traceable to one run.
  export DISCOBOX_BATS_RUN="bats-$$"
  export DISCOBOX_BATS_STATE="$BATS_SUITE_TMPDIR/created"
  : >"$DISCOBOX_BATS_STATE"
}

# teardown_file removes what this suite created, most dependent first, and only
# by the IDs it recorded.
teardown_file() {
  cd "$REPO_ROOT"
  [ -f "${DISCOBOX_BATS_STATE:-}" ] || return 0
  local kind line id
  for kind in sandbox pool provider; do
    while read -r line; do
      [ "${line%% *}" = "$kind" ] || continue
      id="${line#* }"
      cli box "$kind" delete "$id" >/dev/null 2>&1 </dev/null || true
    done <"$DISCOBOX_BATS_STATE"
  done
}

# record notes a resource for teardown as soon as it exists, so a failure
# between creating it and the next assertion still cleans up.
record() {
  echo "$1 $2" >>"$DISCOBOX_BATS_STATE"
}

cli() {
  "$REPO_ROOT/build/disco" --output json "$@"
}

json_get() {
  python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"
}

# wait_for_pool_ready polls the API rather than the server's database: the
# database belongs to the running stack, and neither its path nor its schema is
# this suite's business.
wait_for_pool_ready() {
  local pool_id="$1" deadline=$((SECONDS + 90)) pool_json="" ready schedulable
  while [ "$SECONDS" -lt "$deadline" ]; do
    if pool_json="$(cli box pool get "$pool_id" 2>/dev/null)"; then
      ready="$(printf '%s' "$pool_json" | json_get ready)"
      schedulable="$(printf '%s' "$pool_json" | json_get schedulable)"
      if [ "$ready" = "True" ] && [ "$schedulable" = "True" ]; then
        return 0
      fi
    fi
    sleep 1
  done

  echo "pool did not become ready: $pool_id" >&2
  printf '%s\n' "$pool_json" >&2
  # Diagnostics stay scoped to the pool this suite created.
  docker ps -a --filter "label=discobox.pool_id=$pool_id" \
    --format 'table {{.ID}}\t{{.Image}}\t{{.Status}}\t{{.Names}}' >&2 || true
  local container
  for container in $(docker ps -aq --filter "label=discobox.pool_id=$pool_id"); do
    echo "logs for $container:" >&2
    docker logs --tail 100 "$container" >&2 || true
    echo "journal for $container:" >&2
    docker exec "$container" journalctl --no-pager -n 100 -u discobox-pool-agent.service >&2 || true
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

  run cli box provider create --type docker --name "$DISCOBOX_BATS_RUN-provider" --config "$config"
  [ "$status" -eq 0 ]
  provider_json="$output"
  provider_id="$(printf '%s' "$provider_json" | json_get id)"
  [ -n "$provider_id" ]
  record provider "$provider_id"
  [ "$(printf '%s' "$provider_json" | json_get type)" = "docker" ]

  run cli box pool create "$DISCOBOX_BATS_RUN-pool" --provider "$provider_id"
  [ "$status" -eq 0 ]
  pool_json="$output"
  pool_id="$(printf '%s' "$pool_json" | json_get id)"
  [ -n "$pool_id" ]
  record pool "$pool_id"

  wait_for_pool_ready "$pool_id"

  run cli box sandbox create --name "$DISCOBOX_BATS_RUN-sandbox" --pool "$pool_id" --wait --wait-timeout 90s
  [ "$status" -eq 0 ]
  sandbox_json="$output"
  sandbox_id="$(printf '%s' "$sandbox_json" | json_get id)"
  [ -n "$sandbox_id" ]
  record sandbox "$sandbox_id"
  [ "$(printf '%s' "$sandbox_json" | json_get poolId)" = "$pool_id" ]
  [ "$(printf '%s' "$sandbox_json" | json_get phase)" = "running" ]
  [ "$(printf '%s' "$sandbox_json" | json_get lastOperationStatus)" = "success" ]
}
