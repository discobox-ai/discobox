# The desktop scale, for every shell the sandbox starts.
#
# GDK_SCALE and the rest are read once, when a program starts, so a GUI program
# launched from a terminal needs them in that terminal's environment. The Xfce
# session gets this same file through an EnvironmentFile on its unit; this is
# the other half, for everything an agent or a person starts by hand.
#
# Nothing is needed for interactive shells the way sandbox-direnv.sh needs it:
# these are plain variables and a shell started inside a login shell has already
# inherited them. direnv installs a hook, which is why that file goes further.
#
# `set -a` exports every assignment that follows, which is what lets one plain
# KEY=VALUE file serve both a systemd unit and a shell with no second format.
# It is turned back off immediately: leaving it on would export every variable
# the rest of the login sequence assigns.
[ -r "$HOME/.discobox/desktop/scale.env" ] || return 0
set -a
. "$HOME/.discobox/desktop/scale.env"
set +a
