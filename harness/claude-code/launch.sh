#!/bin/sh
set -eu

# Claude Code supports an explicit auto-memory directory. Supply it at launch
# rather than putting it in the captured user settings file, so reconfigure can
# continue replacing that file without dropping Discobox's storage wiring.
SOURCE_DATA=/.discobox/data-per-source/primary
SHARED_MEMORIES="$SOURCE_DATA/harnesses/claude-code/memories"

if [ -d "$SOURCE_DATA" ]; then
	mkdir -p "$SHARED_MEMORIES"
	exec claude --settings "{\"autoMemoryDirectory\":\"$SHARED_MEMORIES\"}" "$@"
fi

exec claude "$@"
