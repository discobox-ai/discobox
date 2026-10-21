#!/bin/sh
# A browser is resolved two ways and both have to say Chromium: the XDG
# default-applications list (the mimeapps step), and the `x-www-browser`
# alternative that `sensible-browser` and a long tail of scripts go through.
# Debian's package registers the alternative; Chrome for Testing is no
# package, so it is registered here for whichever Chromium the box has.
set -eu

chromium="$(command -v chromium)"
update-alternatives --install /usr/bin/x-www-browser x-www-browser "$chromium" 200
update-alternatives --set x-www-browser "$chromium"
