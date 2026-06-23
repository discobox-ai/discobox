# Process Helper Design

`processhelper` owns the reusable self-reexec helper used to keep child process
trees tied to a parent process without platform-specific parent-death features.

## Pattern

Callers build a command with `CommandContext`. The command re-execs the current
binary with a private helper entry argument and the intended child command after
`--`. The top-level binary must call `HandleEntry` before normal CLI parsing so
helper invocations run the proxy instead of the user-facing command.

The helper treats its stdin as both:

- the byte stream to proxy to the child stdin
- the parent liveness monitor

When stdin reaches EOF, the helper closes child stdin, sends a graceful
termination signal where the platform supports one, waits for the configured
grace period, then force-kills the child process tree if it is still running.

## Boundaries

- Keep the helper transport stdio-only; do not add daemon sockets or persistent
  state here.
- Keep process-tree mechanics inside platform-specific files.
- Do not encode hook, daemon, LSP, or session semantics in this package.
