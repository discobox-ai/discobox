#!/bin/sh
# The commands step's check: each downloaded tool is still linked onto PATH
# from where it is unpacked, and nu is still a login shell.
set -eu

link_command=/usr/local/libexec/discobox/link-command
"$link_command" --check /usr/local/lib/discobox/neovim/bin/nvim
"$link_command" --check /usr/local/lib/discobox/nushell/nu
"$link_command" --check /usr/local/lib/discobox/fresh/fresh
grep -qxF /usr/local/bin/nu /etc/shells
