#!/bin/sh
# /etc/profile covers the login shells the sandbox launches, but not the
# interactive shell a person starts by typing `bash` inside one. That shell
# inherits the environment direnv already exported and would otherwise never
# reload it on a `cd`, so it is hooked from the system-wide interactive rc too.
#
# zsh reads none of /etc/profile, so its hook goes in its own system-wide
# interactive rc. The environment it starts with was already exported by
# discobox-shell; this is what reloads it on a `cd`.
#
# Each is appended once, under its marker, however often this runs.
set -eu

append() {
  file="$1" marker="$2" body="$3"
  if ! grep -qxF "$marker" "$file" 2>/dev/null; then
    printf '\n%s\n%s\n' "$marker" "$body" >> "$file"
  fi
}

append /etc/bash.bashrc '# Discobox: direnv in the interactive shells /etc/profile does not reach.' \
'if [ -r /etc/profile.d/sandbox-direnv.sh ]; then
  . /etc/profile.d/sandbox-direnv.sh
fi'
# shellcheck disable=SC2016 # Written for zsh to expand, not this shell.
append /etc/zsh/zshrc '# Discobox: direnv on every cd.' \
'if command -v direnv >/dev/null 2>&1; then
  eval "$(direnv hook zsh)"
fi'
