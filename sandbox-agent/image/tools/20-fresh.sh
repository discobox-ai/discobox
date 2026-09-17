#!/bin/sh
#---
# name: fresh
# description: the fresh editor, in the box
# key: f
# files: {config.jsonc: .config/fresh/config.json, live_diff.json: .local/share/fresh/orchestrator/state/live_diff.json}
#---
#
# The fresh editor, installed in the image. A script rather than a .yaml because
# opening fresh on a discobox takes one step of setup that is fresh's alone:
# recording that the source directory is trusted.
#
# `files` is what fresh carries in, each copied to the run user's home the first
# time fresh runs in a discobox that has none (ADR 0071 §7–10), from the defaults
# beside this file under fresh/. It is a flow mapping because a commented
# front-matter block loses indentation, so a nested one cannot be written.
#
# config.jsonc is fresh's own configuration. fresh reads config.json, so that is
# where it lands; the local copy is .jsonc because that is what the contents are,
# and the extension is how every editor decides how to color it.
#
# live_diff.json is plugin state, not configuration — fresh has no config key for
# it — and seeding it is the same as having enabled the plugin in an earlier run.
# Strict JSON: it is parsed with serde_json, and a comment would make the whole
# file unreadable, silently.
#
# See docs/adr/0125-tools-are-declared-in-files-the-way-services-are.md.
set -eu

# fresh opens a folder carrying anything that can run code — a package.json, a
# Cargo.toml, an .envrc — as Restricted: no language servers, no environment
# activation. It remembers the decision per folder, in the user's data directory
# rather than in the repository, because a repository must not vouch for itself.
# A discobox is a new folder every time, so it would open Restricted forever;
# the sandbox is the boundary, and the agent already runs this repository's code
# inside it. Recorded only where nothing is recorded yet, so a choice made inside
# the box stands.
#
# The directory name is fresh's `encode_path_for_filename` of the working
# directory, byte for byte: "/" and "\" become "_", alphanumerics and "-" "."
# pass through, "_" becomes %5F so it cannot be mistaken for a separator, every
# other byte is percent-encoded, then runs of "_" collapse and leading ones are
# trimmed. Getting it wrong is silent — the file lands where nothing reads it.
workspace=$(printf %s "$PWD" | od -An -tu1 -v | tr -s ' ' '\n' | grep -v '^$' |
	while read -r b; do
		if [ "$b" -eq 47 ] || [ "$b" -eq 92 ]; then printf _
		elif [ "$b" -eq 95 ]; then printf %%5F
		elif [ "$b" -eq 45 ] || [ "$b" -eq 46 ] ||
			{ [ "$b" -ge 48 ] && [ "$b" -le 57 ]; } ||
			{ [ "$b" -ge 65 ] && [ "$b" -le 90 ]; } ||
			{ [ "$b" -ge 97 ] && [ "$b" -le 122 ]; }; then
			printf "\\$(printf '%03o' "$b")"
		else printf '%%%02X' "$b"
		fi
	done | sed 's/__*/_/g; s/^_*//')
trust="${XDG_DATA_HOME:-$HOME/.local/share}/fresh/workspaces/$workspace/trust.json"
if [ ! -e "$trust" ]; then
	mkdir -p "$(dirname "$trust")"
	printf '{\n  "level": "trusted"\n}\n' > "$trust"
fi

# The "." is not decoration. fresh opens the directory — with its file tree, and
# the project as a workspace it can remember — only when it is given exactly one
# argument and that argument is a directory; with none it comes up on an empty
# [No Name] buffer and no tree. The session lands in the primary source
# directory, so "." is that directory. Arguments of the caller's own replace it.
if [ "$#" -eq 0 ]; then
	set -- .
fi
exec fresh "$@"
