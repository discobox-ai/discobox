#!/usr/bin/env bash
# Run shellcheck over every shell script in the working tree.
#
# A shell script is a file named *.sh, *.bash, or *.bats, one whose first line
# is a sh or bash shebang (the image's extensionless helpers, such as
# sandbox-agent/image/harness-run), or .envrc. Files sourced rather than run
# have no shebang and say which shell they are with a `# shellcheck shell=`
# directive instead.
#
# The whole tree is checked every time rather than the changed files: it takes a
# couple of seconds, and a hook that checks only what changed cannot report a
# failure it saw earlier as fixed.
#
# Inline scripts in Taskfile.yml and the Dockerfiles' RUN steps are not
# reached; shellcheck reads files, not YAML or Dockerfiles.
#
# Called by `go tool task check:shell` and by the hook of the same name.

set -euo pipefail

cd "${DISCOBOX_WORKSPACE:-$(git rev-parse --show-toplevel)}"

# The working tree rather than the index: a script the hook fires on is usually
# one nobody has `git add`ed yet, and a tracked one deleted but not yet staged
# is still in the index. Ignored files stay out either way.
{
	git ls-files --cached --others --exclude-standard -- '*.sh' '*.bash' '*.bats' .envrc
	# -n prefixes each match with its line number; only a match on line 1 is a
	# shebang. The extensions above are left out so nothing is listed twice.
	# git grep exits 1 when nothing matches, which is not a failure here.
	{ git grep --untracked -nIE '^#!(/usr/bin/env |/bin/|/usr/bin/)(ba)?sh( |$)' -- \
		':!*.sh' ':!*.bash' ':!*.bats' || [ $? -eq 1 ]; } |
		awk -F: '$2 == 1 { print $1 }'
} | sort -u | while IFS= read -r file; do
	if [ -e "$file" ]; then
		printf '%s\0' "$file"
	fi
done | xargs -0 shellcheck
