#---
# id: ai.discobox.desktop
# name: Desktop
# description: The sandbox's graphical desktop, in a browser tab.
# port: 6900
# protocol: http
# start: never
#---
#
# A declaration, not a script. `start: never` is what says so, and it is why
# this file has no shebang, no executable bit, and nothing to run.
#
# The desktop viewer is started by systemd, on demand, when something connects
# to 6900 — see sandbox-agent/image/systemd/discobox-desktop.socket. That is
# also why the port has to be declared at all, twice over:
#
#   - The port watcher only sees sockets the sandbox user owns, and a .socket
#     unit's listener is bound by pid 1. It is invisible to discovery.
#   - Classifying a port means connecting to it, and connecting to this one is
#     the activation. A probe would start an X server, a window manager, a VNC
#     server and the viewer, in every sandbox, on a timer — the whole point of
#     socket-activating it, undone by the mechanism meant to describe it.
#     Stating the protocol is what lets the port be reported without ever being
#     touched.
#
# The id is stated rather than derived from this filename because it is what a
# client matches on to give the desktop its own affordance instead of listing it
# as an HTTP port on 6900. Renaming this file must not change it, and the
# `ai.discobox.` namespace is reserved so a repository cannot declare it.
#
# See docs/adr/0094-an-image-declares-services-in-the-format-a-repository-does.md.
