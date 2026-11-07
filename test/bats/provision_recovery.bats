#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats exits on failed cd.
# Real dpkg interruption on the dev pool. Only the recorded box is destroyed.

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
  local id container
  id="$(cat "$BATS_SUITE_TMPDIR/sandbox" 2>/dev/null || true)"
  [ -n "$id" ] || return 0
  container="$(docker ps -aq --filter "name=-$id")"
  if [ -n "$container" ]; then
    docker logs "$container" >"$BATS_SUITE_TMPDIR/container.log" 2>&1 || true
    docker exec "$container" cat /var/log/discobox/provision/recovery.log >&3 2>&1 || true
  fi
  cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
}

@test "restart during background apt configuration recovers and unblocks default services" {
  local id container state deadline
  # Isolate the transaction from unrelated downloads and toolchain installs.
  mkdir -p "$BATS_TEST_TMPDIR/app/.discobox"
  cat >"$BATS_TEST_TMPDIR/app/.discobox/settings.json" <<'SETTINGS'
{"features":{"brew":false,"browser":false,"build":false,"bun":false,"cli":false,"desktop":false,"direnv":false,"discobox-cli":false,"docker":false,"go":false,"mise":false,"nix":false,"node":false,"python":false,"review":false,"rust":false,"shells-editors":false}}
SETTINGS
  git -C "$BATS_TEST_TMPDIR/app" init -q
  git -C "$BATS_TEST_TMPDIR/app" add .
  git -C "$BATS_TEST_TMPDIR/app" -c user.name=Test -c user.email=test@discobox.internal commit -qm 'Isolate recovery regression'
  id="$(cli run -d -H shell "$BATS_TEST_TMPDIR/app" -o json | jq -er .id)"
  echo "$id" >"$BATS_SUITE_TMPDIR/sandbox"
  # Wait for the normal platform before adding a controlled background feature.
  run timeout 600 env HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" exec "$id" -- discobox-provision wait core
  echo "$output"
  [ "$status" -eq 0 ]
  container="$(docker ps -q --filter "name=-$id")"
  [ -n "$container" ]
  docker exec -i "$container" sh -s <<'SETUP'
set -eu
feature=/opt/discobox/bundles/by-name/discobox-harness-shell/features/recovery
mkdir -p "$feature" /var/lib/discobox/recovery-deb/DEBIAN /usr/local/share/discobox/services
cat >/var/lib/discobox/recovery-deb/DEBIAN/control <<'CONTROL'
Package: discobox-recovery-test
Version: 1.0
Architecture: all
Maintainer: Discobox <test@discobox.internal>
Description: Controlled provisioning interruption
CONTROL
cat >/var/lib/discobox/recovery-deb/DEBIAN/postinst <<'POSTINST'
#!/bin/sh
set -eu
if [ ! -e /var/lib/discobox/recovery-entered ]; then
  touch /var/lib/discobox/recovery-entered
  # The first configure is deliberately interrupted by a real box restart.
  sleep 600
fi
touch /var/lib/discobox/recovery-configured
POSTINST
chmod 755 /var/lib/discobox/recovery-deb/DEBIAN/postinst
dpkg-deb --build /var/lib/discobox/recovery-deb /var/lib/discobox/recovery.deb
cat >"$feature/provision.yaml" <<'SPEC'
apiVersion: discobox.ai/provision/v1
async: true
steps:
  - name: install
    apt: true
    run: install.sh
SPEC
cat >"$feature/install.sh" <<'SCRIPT'
#!/bin/sh
set -eu
DEBIAN_FRONTEND=noninteractive apt-get install -y /var/lib/discobox/recovery.deb
SCRIPT
chmod 755 "$feature/install.sh"
mkdir -m 1777 /var/lib/discobox/recovery-service-state
cat >/usr/local/share/discobox/services/recovery.sh <<'SERVICE'
#!/bin/sh
#---
# id: recovery-test
# name: Recovery test
#---
touch /var/lib/discobox/recovery-service-state/started
exec sleep 600
SERVICE
chmod 755 /usr/local/share/discobox/services/recovery.sh
SETUP
  cli restart "$id"
  deadline=$((SECONDS + 180))
  until docker exec "$container" test -e /var/lib/discobox/recovery-entered; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 0.2
  done
  state="$(docker exec "$container" cat /run/discobox/provision.json)"
  echo "$state"
  jq -e '.ready == true and .features.recovery.state == "provisioning"' <<<"$state"
  docker exec "$container" test ! -e /var/lib/discobox/recovery-service-state/started
  # Cut power while apt/dpkg holds the transaction, then start via the product.
  docker kill "$container"
  cli restart "$id"
  run timeout 180 env HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" exec "$id" -- discobox-provision wait recovery
  echo "$output"
  [ "$status" -eq 0 ]
  docker exec "$container" test -e /var/lib/discobox/recovery-configured
  deadline=$((SECONDS + 30))
  until state="$(docker exec "$container" cat /run/discobox/provision.json)" &&
    jq -e '.ready == true and .state == "ready" and .features.recovery.state == "ready"' <<<"$state"; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 0.2
  done
  echo "$state"
  cli admin service start "$id" recovery-test
  deadline=$((SECONDS + 30))
  until docker exec "$container" test -e /var/lib/discobox/recovery-service-state/started; do
    [ "$SECONDS" -lt "$deadline" ] || return 1
    sleep 0.2
  done
}
