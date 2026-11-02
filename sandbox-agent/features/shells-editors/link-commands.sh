#!/bin/sh
# The downloaded tools' commands, linked from where each is unpacked
# (link-command, which moves aside what the box had at the name). Neovim
# finds its runtime files from its own real path, so the link is enough.
set -eu

link_command=/usr/local/libexec/discobox/link-command
"$link_command" /usr/local/lib/discobox/neovim/bin/nvim
"$link_command" /usr/local/lib/discobox/nushell/nu
"$link_command" /usr/local/lib/discobox/fresh/fresh
# By the name on PATH alone: add-shell would add the path it resolves to as
# well.
if ! grep -qxF /usr/local/bin/nu /etc/shells; then
  echo /usr/local/bin/nu >> /etc/shells
fi
