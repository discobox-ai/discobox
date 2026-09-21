#!/usr/bin/env bash
# Print watchnbuild's failure reports, and fail if there are any.
#
# `task dev` runs watchnbuild in a terminal nobody is necessarily watching —
# least of all an agent, which never sees it at all. A failed build or run
# leaves wnb-build-failed.txt or wnb-run-failed.txt in the repository root
# instead, and this turns those files into a hook failure, so
# `task check-hooks` reports a broken dev build the same way it reports a
# broken test.
#
# `task dev:cli` is not reliably covered. wnb's report names are fixed and
# written to its working directory, and both loops run from the repository
# root, so they share one wnb-build-failed.txt: whichever loop builds
# successfully removes it, including a report the other loop wrote. A CLI build
# that succeeds while `task dev`'s is still broken turns this check green.
# Giving each loop its own report needs a report path watchnbuild does not yet
# let a config set.
#
# Called by `go tool task check:dev-build` and by the hook of the same name.
#
# Exiting 0 when no report exists is the point, not an edge case: wnb removes a
# report once that step works again, the deletion is a file change like any
# other, and the hook that runs on it has to be able to succeed.

set -euo pipefail

cd "${DISCOBOX_WORKSPACE:-$(git rev-parse --show-toplevel)}"

# Unmatched globs expand to themselves without this, so a repository with no
# reports would "find" a file named wnb-*-failed.txt.
shopt -s nullglob
reports=(wnb-*-failed.txt)

if [ ${#reports[@]} -eq 0 ]; then
    exit 0
fi

for report in "${reports[@]}"; do
    printf '===> %s\n\n' "$report"
    cat -- "$report"
    printf '\n'
done

# wnb writes each report when the step fails and removes it when the step
# succeeds, so one being here means the dev loop is broken right now.
echo "Dev loop build or run is failing. Fix it, or stop the wnb that wrote this and delete the report."
exit 1
