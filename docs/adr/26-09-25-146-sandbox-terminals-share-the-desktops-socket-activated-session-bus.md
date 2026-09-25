# 26-09-25-146 — Sandbox terminals share the desktop's socket-activated session bus

- **Status**: Accepted
- **Date**: 2026-09-25
- **Relates to**: [ADR 26-09-05-409](26-09-05-409-an-image-declares-services-in-the-format-a-repository-does.md),
  which stopped the port probe from starting the desktop; this closes the other
  route found since.

## Context

The desktop is socket-activated so that a sandbox nobody looks at runs no X
server. Two sockets start it: the viewer's port 6900, and `x11-display.socket`
on `/tmp/.X11-unix/X0`. X starting pulls in the Xfce session and the viewer as
companions, so any connection to `:0` brings up the whole desktop.

Every sandbox's desktop was starting within seconds of boot anyway. Traced
through the journal and cgroups, then reproduced against a logging fake display:

1. Every exec gets `DISPLAY=:0` from the image manifest's env, and no
   `DBUS_SESSION_BUS_ADDRESS`.
2. Interactive Claude Code runs `gh auth token` at startup; `gh` looks for its
   token in the Secret Service over the session bus.
3. A D-Bus client with no bus address and a `DISPLAY` autolaunches one:
   `dbus-launch --autolaunch` connects to the X server to find or register the
   bus. That connection is what hit `x11-display.socket`.

Any D-Bus client in a terminal does the same — libdbus and godbus both
autolaunch. The session bus the desktop already had,
`discobox-desktop-bus.service`, was deliberately not exported to terminals: it
ran only while the desktop did, so the address would have been dead the rest of
the time.

## Decision

1. **The desktop's bus becomes the sandbox user's session bus**, renamed
   `discobox-session-bus.service`, and is socket-activated by
   `discobox-session-bus.socket` on `/run/discobox/session/bus`. Reaching it
   starts only `dbus-daemon`, never X.
2. **Every sandbox terminal is given its address** as `DBUS_SESSION_BUS_ADDRESS`
   in the base image manifest's env, beside `DISPLAY`. The path is fixed, so the
   value is static.
3. The socket is created 0600 and owned by the sandbox user, through a boot
   drop-in as the daemon's `User=` already was. An exec running as another user
   gets a refused connection — an error in the client, not an autolaunch.

With the address set, the same interactive `claude` start that brought the
desktop up leaves X down; `xfconf-query` from a terminal reads the xfconf the
Xfce session uses.

## Alternatives rejected

- **A logind login session per exec** (`PAMName=login` on the exec's transient
  unit, logind unmasked, `libpam-systemd`). It works in the container and
  yields `XDG_RUNTIME_DIR` and a user bus, but `pam_systemd` moves the session
  leader into `session-N.scope`, out of `discobox-exec-*.service` — the exec's
  shell and everything under it leave the unit that exec stop, resource
  collection and the exec watcher key on. It also needs `User=` on the unit,
  while `exec-shim` runs as root and drops privileges itself.
- **The systemd user manager without logind** (`user@<uid>.service` plus
  `dbus-user-session`). It runs given an `XDG_RUNTIME_DIR` drop-in and fixes the
  autolaunch too, but it is a second session bus beside the desktop's — a GUI
  program started from a terminal and one started from the panel would see
  different buses — and it runs `user@.service` in a configuration systemd does
  not design for, for `systemctl --user`, which nothing here uses.
- **Not giving terminals `DISPLAY`.** Nothing could reach X by accident, but a
  GUI program started from a terminal would no longer open on the desktop.
- **An unusable bus address** (`DBUS_SESSION_BUS_ADDRESS` pointing at nothing).
  Stops the autolaunch and breaks every legitimate D-Bus use from a terminal.

## Consequences

- A D-Bus-activated service a terminal reaches (xfconfd, Thunar) runs under
  `discobox-session-bus.service`, as the desktop's already did, not under the
  exec that asked for it. Activating a GUI one still starts the desktop, which
  is what a person asking for a GUI program wants.
- `DISPLAY` without a live bus address is what reopens this. The two env
  entries go together.
