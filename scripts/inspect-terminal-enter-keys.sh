#!/usr/bin/env bash

set -euo pipefail

mode=${1:-all}
case "$mode" in
  all | both | plain | legacy | kitty) ;;
  *) echo "usage: $0 [all|plain|legacy|kitty]" >&2; exit 2 ;;
esac

tty=/dev/tty
if [[ ! -r "$tty" || ! -w "$tty" ]]; then
  echo "this program needs a controlling terminal" >&2
  exit 1
fi

saved_stty=$(stty -g <"$tty")
tmp_dir=$(mktemp -d)
kitty_pushed=false

cleanup() {
  stty "$saved_stty" <"$tty" 2>/dev/null || true
  printf '\033[>4m' >"$tty"
  [[ "$kitty_pushed" == false ]] || printf '\033[<u' >"$tty"
  rm -r "$tmp_dir"
  printf '\n' >"$tty"
}
trap cleanup EXIT HUP INT TERM
stty raw -echo <"$tty"

capture() {
  local label=$1 bytes="$tmp_dir/bytes" hex chars
  : >"$bytes"
  printf '%-20s press now: ' "$label" >"$tty"

  # Let the TTY driver perform the timeout. Wrapping the read in `timeout`
  # moves it out of a tmux pane's foreground process group, where it cannot
  # receive terminal input.
  stty min 0 time 50 <"$tty"
  dd if="$tty" of="$bytes" bs=64 count=1 status=none
  if [[ ! -s "$bytes" ]]; then
    printf 'no bytes received within 5 seconds\r\n' >"$tty"
    return
  fi
  stty min 0 time 1 <"$tty"
  dd if="$tty" of="$bytes" bs=64 count=1 oflag=append conv=notrunc status=none

  hex=$(od -An -v -t x1 "$bytes" | tr -s ' ' | sed 's/^ //')
  chars=$(od -An -v -c "$bytes" | tr -s ' ' | sed 's/^ //')
  printf 'hex: %-28s chars: %s\r\n' "$hex" "$chars" >"$tty"
}

run_keys() {
  capture 'Shift-Enter'
  capture 'Ctrl-Enter'
  capture 'Ctrl-J'
}

run_plain() {
  printf '\r\n--- Plain terminal input; no extended keyboard mode ---\r\n' >"$tty"
  printf '\033[>u\033[>4m' >"$tty"
  kitty_pushed=true
  run_keys
  printf '\033[<u' >"$tty"
  kitty_pushed=false
}

run_legacy() {
  printf '\r\n--- xterm modifyOtherKeys 2; Kitty disabled ---\r\n' >"$tty"
  printf '\033[>u\033[>4;2m' >"$tty"
  kitty_pushed=true
  run_keys
  printf '\033[>4m\033[<u' >"$tty"
  kitty_pushed=false
}

run_kitty() {
  printf '\r\n--- Kitty keyboard protocol; disambiguate keys ---\r\n' >"$tty"
  printf '\033[>4m\033[>1u' >"$tty"
  kitty_pushed=true
  run_keys
  printf '\033[<u' >"$tty"
  kitty_pushed=false
}

[[ "$mode" == all || "$mode" == plain ]] && run_plain
[[ "$mode" == all || "$mode" == both || "$mode" == legacy ]] && run_legacy
[[ "$mode" == all || "$mode" == both || "$mode" == kitty ]] && run_kitty
