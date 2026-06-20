# Discobox Session CLI Design

`discobox-session` is the user-facing entrypoint for the sessions module. It
resolves Git/session context, starts the daemon on demand, forwards commands to
the daemon, and owns local terminal behavior during attach.

Command surface:

- `daemon`: run the foreground daemon
- `daemon status`: show daemon status
- `daemon shutdown`: ask daemon to exit
- `agents` / `supported-agents`: list supported agents and their configured
  commands
- `create <agent> [-- agent args...]`: start a new PTY-backed session
- `list` / `ls`: list live daemon sessions
- `attach [session-id]`: attach to a running session; without an ID, attach the
  most recently created running session

Detach from an attached session with `ctrl+p q`.

Attach sends the current terminal size and then `ctrl+l` to ask terminal UIs to
redraw. The daemon does not yet replay scrollback from before the attach.
