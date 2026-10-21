#!/bin/sh
# The setuid helper Chromium's sandbox uses where the kernel allows it, at the
# name the browser looks for beside itself; the build ships it unprivileged
# as chrome_sandbox. And the build's own icon, under the name the desktop
# entries give it.
set -eu

dir=/usr/local/lib/discobox/chromium
install -o root -g root -m 4755 "$dir/chrome_sandbox" "$dir/chrome-sandbox"
install -D -m 0644 "$dir/product_logo_48.png" /usr/share/icons/hicolor/48x48/apps/chromium.png
