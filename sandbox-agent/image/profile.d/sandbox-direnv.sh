# direnv, for every shell the sandbox starts.
#
# Sourced from the two places that between them cover every shell: /etc/profile
# runs this for login shells (a terminal is `bash -l`, a service is
# `bash -lc <script>`), and /etc/bash.bashrc runs it for the interactive shells
# a person starts inside one. Both fire for an interactive login shell, which is
# harmless -- `direnv hook bash` only adds itself to PROMPT_COMMAND once.
#
# Named to sort after sandbox-dev-tools.sh, which puts the Nix profile on PATH:
# /etc/profile sources profile.d in glob order, and a direnv installed there has
# to be findable before this runs.
[ -n "${BASH_VERSION:-}" ] || return 0
command -v direnv >/dev/null 2>&1 || return 0

case $- in
  # The prompt hook is what makes `cd` reload the environment, and it is all an
  # interactive shell needs: it fires before the first prompt, so a terminal
  # opened in a source tree has the .envrc loaded by the time it is usable.
  *i*) eval "$(direnv hook bash)" ;;
  # A shell that prints no prompt never runs PROMPT_COMMAND, so a service script
  # would get none of its repository's .envrc. Export once instead, here,
  # against the directory the shell was started in.
  *) eval "$(direnv export bash)" ;;
esac
