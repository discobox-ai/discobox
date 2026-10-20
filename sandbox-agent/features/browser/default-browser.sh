#!/bin/sh
# Debian resolves a browser two ways and both have to say Chromium: the XDG
# default-applications list (the mimeapps step), and the `x-www-browser`
# alternative that `sensible-browser` and a long tail of scripts go through.
#
# The initial-preferences file turns off Chromium's own "make me your
# default?" infobar, which is noise on a desktop where the question is already
# answered. Both filenames are written: Debian's packaging has used
# `master_preferences` for years and upstream renamed it to
# `initial_preferences`, and whichever this Chromium reads, the other is inert.
set -eu

update-alternatives --set x-www-browser /usr/bin/chromium
mkdir -p /etc/chromium
printf '%s\n' '{"browser":{"check_default_browser":false},"distribution":{"import_bookmarks":false,"make_chrome_default":false,"suppress_first_run_bubble":true,"skip_first_run_ui":true}}' \
  | tee /etc/chromium/master_preferences > /etc/chromium/initial_preferences
