#!/bin/sh
# Every git in the box asks discobox-access for an HTTPS host's credential: it
# answers with the placeholder of the one credential the box holds whose
# routes name that host, so git clone and git push of a repository a granted
# token covers need no setup (access/gitcredential.go). In the system config,
# for every URL, so it applies to every user and checkout and comes before
# a user's own helpers, which git still asks when this one has no answer; a
# source's origin clears it for its own URL with the agent's helper
# (sandbox-agent/sourceconverge).
#
# Written as text rather than with `git config --system`: the box may not
# have git yet (core's git step installs one in the background), and the
# config is git's whenever it arrives. Appended once, so a re-apply leaves
# the file as it was and whatever else the image put there stays.
set -eu

helper='!/usr/local/bin/discobox-access git-credential'
config=/etc/gitconfig

if [ -f "$config" ] && grep -qF "helper = $helper" "$config"; then
    exit 0
fi
# A file that does not end its last line would run that line into ours.
if [ -s "$config" ] && [ -n "$(tail -c 1 "$config")" ]; then
    echo >> "$config"
fi
printf '[credential]\n\thelper = %s\n' "$helper" >> "$config"
