#!/bin/sh
# git is 2.46 or later: the first that takes a bearer credential.
version="$(git --version 2>/dev/null | sed -n 's/^git version \([0-9]*\)\.\([0-9]*\).*/\1 \2/p')"
[ -n "$version" ] || exit 1
# shellcheck disable=SC2086 # major and minor, as words
set -- $version
[ "$1" -gt 2 ] || { [ "$1" -eq 2 ] && [ "$2" -ge 46 ]; }
