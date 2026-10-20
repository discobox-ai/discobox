#!/bin/sh
# The backdrop and the panel's menu button, rasterized from the same brand
# SVGs the viewer's page is drawn with. Two details:
#
#   - librsvg only follows a reference that stays inside the document's own
#     directory, which is why the wallpaper and the wordmark it draws are
#     copied into one directory to render.
#   - the mark is rendered onto a page larger than itself, because the panel
#     scales a button icon to the full height it is given and the mark would
#     otherwise sit on the panel with its feet on both edges.
set -eu

brand="${DISCOBOX_PROVISION_FEATURE_DIR:?}/brand"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p /usr/local/share/discobox/desktop
cp wallpaper.svg "$brand/wordmark-white.svg" "$work/"
rsvg-convert -w 2560 -h 1440 -o /usr/local/share/discobox/desktop/wallpaper.png "$work/wallpaper.svg"
rsvg-convert --page-width 160 --page-height 160 --top 16 --left 18 -h 128 \
  -o /usr/local/share/discobox/desktop/mark.png "$brand/logo-purple.svg"
