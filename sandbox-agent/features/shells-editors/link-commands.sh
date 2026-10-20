#!/bin/sh
# The downloaded tools' commands, linked from where each is unpacked. Neovim
# finds its runtime files from its own real path, so the link is enough.
set -eu

ln -sfn /usr/local/lib/discobox/neovim/bin/nvim /usr/local/bin/nvim
ln -sfn /usr/local/lib/discobox/nushell/nu /usr/local/bin/nu
ln -sfn /usr/local/lib/discobox/fresh/fresh /usr/local/bin/fresh
# By the name on PATH alone: add-shell would add the path it resolves to as
# well.
if ! grep -qxF /usr/local/bin/nu /etc/shells; then
  echo /usr/local/bin/nu >> /etc/shells
fi
