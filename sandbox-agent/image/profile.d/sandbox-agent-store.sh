# The agent version this sandbox runs, and the pool's store of versions to run
# (ADR 0114).
#
# Two things happen here, and neither of them waits on the network:
#
#   - `pin` points ~/.local/bin at the newest version already in the store, once
#     in this sandbox's life. The link is on the sandbox's own data volume and
#     ahead of /usr/bin on PATH, so the version survives a restart and the
#     harness launcher needs no changes at all — it still just runs `claude`.
#   - `refresh` is detached and stamped: it returns immediately unless the
#     store's check window has passed, so ten sandboxes starting is one registry
#     query, not ten. A sandbox stops itself when idle (ADR 0108), so the store
#     is caught up by whoever logs in next rather than by a timer that would
#     need a box to stay up for twelve hours.
#
# The login sequence is where this belongs: the harness command is typed into a
# terminal's login shell (ADR 0027), so this runs before the agent does, in
# every harness image including ones Discobox does not ship.
[ -r /usr/local/libexec/discobox/agent.conf ] || return 0
command -v discobox-agent-store >/dev/null 2>&1 || return 0

discobox-agent-store pin >/dev/null 2>&1 || true

# setsid so the fetch is not in this shell's process group: a login shell that
# exits, or a terminal that goes away, must not take a download with it.
if command -v setsid >/dev/null 2>&1; then
	(setsid discobox-agent-store refresh >/dev/null 2>&1 </dev/null &) || true
else
	(discobox-agent-store refresh >/dev/null 2>&1 </dev/null &) || true
fi
