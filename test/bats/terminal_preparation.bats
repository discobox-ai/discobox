#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats exits on a failed cd.
# Drives the existing dev stack, like worktree_git.bats. A fresh box has no
# provisioned platform or primary terminal. Own and purge only its recorded ID.

setup_file() {
  REPO_ROOT="$(cd "${BATS_TEST_FILENAME%/*}/../.." && pwd)"
  export REPO_ROOT
  export DISCOBOX_BATS_SERVER="http://127.0.0.1:${PORT:-18080}"
  curl -fsS "$DISCOBOX_BATS_SERVER/openapi.yaml" >/dev/null 2>&1 || skip "development server is required"
  export DISCOBOX_BATS_HOME="$BATS_SUITE_TMPDIR/home"
  mkdir -p "$DISCOBOX_BATS_HOME"
  cli admin harness ls -o json | jq -e '.harnessConfigs[] | select(.slug == "shell")' >/dev/null || skip "shell harness is required"
}

cli() {
  HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" "$@"
}

teardown_file() {
  local id
  id="$(cat "$BATS_SUITE_TMPDIR/sandbox" 2>/dev/null || true)"
  [ -n "$id" ] || return 0
  cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
}

@test "cold terminal preparation differs from an unknown terminal" {
  local id project state code saw_preparing=false deadline=$((SECONDS + 180))
  id="$(cli run -d -H shell --no-source -o json | jq -er .id)"
  echo "$id" >"$BATS_SUITE_TMPDIR/sandbox"
  project="$(cli admin box get "$id" -o json | jq -er .projectId)"
  # Observe the actual agent provisioning window, not only pool/container boot.
  while [ "$SECONDS" -lt "$deadline" ]; do
    state="$(cli admin box get "$id" -o json)"
    if jq -e '.runtime.runtimeState == "running" and .runtime.agentStatus.provision.ready == false' <<<"$state" >/dev/null; then
      saw_preparing=true
      [ "$(jq -r .runtime.displayState <<<"$state")" = starting ]
      run cli capture "$id" --trim
      echo "$output"
      [[ "$output" != *"sandbox exec not found"* ]]
      [[ "$output" != *"sandbox exec has ended"* ]]
      [ "$status" -eq 0 ] || [[ "$output" == *prepar* ]]
      break
    fi
    sleep 0.2
  done
  [ "$saw_preparing" = true ]
  # A real missing ID is still 404 while primary preparation is in progress.
  code="$(curl -sS -o "$BATS_TEST_TMPDIR/unknown" -w '%{http_code}' "$DISCOBOX_BATS_SERVER/api/projects/$project/sandboxes/$id/execs/exec-does-not-exist/screen")"
  [ "$code" = 404 ]
  grep -q 'sandbox exec not found' "$BATS_TEST_TMPDIR/unknown"
  # Command creation waits on readiness, then runs without a primary-ID lookup.
  run cli exec "$id" -- sh -c 'echo terminal-prepared'
  echo "$output"
  [ "$status" -eq 0 ]
  [[ "$output" == *terminal-prepared* ]]
  # Capture becomes usable once the primary has its shim, independently of the
  # plain command. Retry the explicit preparation response, never a 404.
  while [ "$SECONDS" -lt "$deadline" ]; do
    run cli capture "$id" --trim
    [ "$status" -ne 0 ] || break
    echo "$output"
    [[ "$output" == *prepar* ]]
    sleep 0.2
  done
  [ "$status" -eq 0 ]
}
