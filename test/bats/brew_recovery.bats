#!/usr/bin/env bats
# shellcheck disable=SC2164 # bats exits on failed cd.
# Real brew, real feature states, and a prefix persisted across box restarts.

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
    docker exec "$container" cat /var/log/discobox/provision/brew.log >&3 2>&1 || true
  fi
  cli admin box purge "$id" >/dev/null 2>&1 </dev/null || true
}

wait_brew() {
  run timeout 600 env HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" exec "$id" -- discobox-provision wait brew
  echo "$output"
  [ "$status" -eq 0 ]
  docker exec "$container" cat /run/discobox/provision.json | jq -e '.features.brew.state == "ready"'
}

@test "brew recovers stale ownership and interrupted clone and vendor-install without losing kegs" {
  local id container phase
  mkdir -p "$BATS_TEST_TMPDIR/app/.discobox"
  cat >"$BATS_TEST_TMPDIR/app/.discobox/settings.json" <<'SETTINGS'
{"features":{"browser":false,"build":false,"bun":false,"cli":false,"desktop":false,"direnv":false,"discobox-cli":false,"docker":false,"go":false,"mise":false,"nix":false,"node":false,"python":false,"review":false,"rust":false,"shells-editors":false}}
SETTINGS
  git -C "$BATS_TEST_TMPDIR/app" init -q
  git -C "$BATS_TEST_TMPDIR/app" add .
  git -C "$BATS_TEST_TMPDIR/app" -c user.name=Test -c user.email=test@discobox.internal commit -qm 'Isolate brew recovery regression'
  id="$(cli run -d -H shell "$BATS_TEST_TMPDIR/app" -o json | jq -er .id)"
  echo "$id" >"$BATS_SUITE_TMPDIR/sandbox"
  run timeout 600 env HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" exec "$id" -- discobox-provision wait core
  echo "$output"
  [ "$status" -eq 0 ]
  container="$(docker ps -q --filter "name=-$id")"
  [ -n "$container" ]
  wait_brew
  # Install the source under test explicitly, also allowing a baseline script
  # to demonstrate that the same stale lock case fails before the fix.
  docker cp "${BREW_INSTALL_SCRIPT:-$REPO_ROOT/sandbox-agent/features/brew/install-brew.sh}" "$container:/opt/discobox/bundles/by-name/discobox-platform/features/brew/install-brew.sh"
  docker exec -i "$container" sh -s <<'SETUP'
set -eu
p=/home/linuxbrew/.linuxbrew
mkdir -p "$p/Cellar/recovery/1.0" "$p/opt" "$p/var/database" /var/lib/brew-recovery
printf 'keep keg\n' >"$p/Cellar/recovery/1.0/data"
printf 'keep service\n' >"$p/var/database/data"
printf 'outside\n' >/var/lib/brew-recovery/outside
chmod 600 /var/lib/brew-recovery/outside "$p/var/database/data"
chmod 00700 "$p/var/database"
ln -s ../Cellar/recovery/1.0 "$p/opt/recovery"
ln -s /var/lib/brew-recovery/outside "$p/Cellar/recovery/1.0/outside"
# A released advisory lock remains as a file after its process has died.
touch "$p/var/homebrew/locks/vendor-install-ruby"
chown -R root:root "$p/var" "$p/Cellar" "$p/opt"
chown nobody "$p/Cellar/recovery/1.0/data"
chmod 2755 "$p/var" "$p/var/homebrew"
chmod 2775 "$p/var/homebrew/locks"
printf '\n# exercise persisted ownership repair\n' >>/opt/discobox/bundles/by-name/discobox-platform/features/brew/install-brew.sh
SETUP
  cli restart "$id"
  wait_brew
  docker exec "$container" stat -c %z /home/linuxbrew/.linuxbrew/Cellar/recovery/1.0/data >"$BATS_TEST_TMPDIR/keg-ctime"
  for phase in clone vendor; do
    # Execute the real operation, then fail the script immediately afterwards.
    docker exec -i "$container" sh -s -- "$phase" <<'INJECT'
set -eu
phase=$1
if [ "$phase" = clone ]; then tool=git; else tool=setpriv; fi
cat >"/usr/local/bin/$tool" <<WRAPPER
#!/bin/sh
/usr/bin/$tool "\$@"
result=\$?
[ "\$result" -eq 0 ] || exit "\$result"
case "\$*" in
  *clone*|*vendor-install*) exit 73 ;;
esac
WRAPPER
chmod 755 "/usr/local/bin/$tool"
printf '\n# fail after %s\n' "$phase" >>/opt/discobox/bundles/by-name/discobox-platform/features/brew/install-brew.sh
INJECT
    cli restart "$id"
    run timeout 600 env HOME="$DISCOBOX_BATS_HOME" "$REPO_ROOT/build/discobox" --server "$DISCOBOX_BATS_SERVER" exec "$id" -- discobox-provision wait brew
    echo "$output"
    [ "$status" -ne 0 ]
    docker exec "$container" cat /run/discobox/provision.json | jq -e '.features.brew.state == "failed"'
    docker exec "$container" grep 'exit status 73' /var/log/discobox/provision/brew.log
    # Removing the interruption alone lets the unchanged, failed step run again.
    docker exec "$container" rm -f /usr/local/bin/git /usr/local/bin/setpriv
    cli restart "$id"
    wait_brew
  done
  docker exec "$container" stat -c %z /home/linuxbrew/.linuxbrew/Cellar/recovery/1.0/data >"$BATS_TEST_TMPDIR/keg-ctime-after"
  cmp "$BATS_TEST_TMPDIR/keg-ctime" "$BATS_TEST_TMPDIR/keg-ctime-after"
  docker exec -i "$container" sh -s <<'CHECK'
set -eu
p=/home/linuxbrew/.linuxbrew
[ -z "$(find "$p/Homebrew" "$p/var/homebrew" ! -user root -print -quit)" ]
[ "$(cat "$p/Cellar/recovery/1.0/data")" = 'keep keg' ]
[ "$(cat "$p/var/database/data")" = 'keep service' ]
[ "$(stat -c '%g:%a' "$p/var/database")" = '0:700' ]
[ "$(stat -c '%g:%a' "$p/var/database/data")" = '0:600' ]
[ "$(stat -c '%g:%a' /var/lib/brew-recovery/outside)" = '0:600' ]
[ "$(stat -c '%U:%G' "$p/Cellar/recovery/1.0/data")" = nobody:brew ]
setpriv --reuid daemon --regid brew --clear-groups sh -c 'echo writable >>/home/linuxbrew/.linuxbrew/Cellar/recovery/1.0/data'
"$p/bin/brew" --version
CHECK
}
