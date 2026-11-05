# Repository startup scripts

On Linux boxes, a repository may provide an executable
`.discobox/scripts/pre-harness` to run interactively before the selected
harness. This script runs automatically as the box user with the primary
terminal's environment, credential bindings, working directory and terminal.
Read repository code before running a source you do not trust.

Only the primary source supplies the script. Its path is relative to that
checkout root, even when commands start in a subdirectory. It must be a regular
executable with a valid interpreter; symlinks must resolve inside the checkout.

Exit zero to continue to the selected harness. Any failure, invalid executable,
or interrupt prevents harness launch and leaves the login shell available to
repair the script. With the shell harness, success returns to that existing
shell. The script receives no harness prompt or resume arguments.

The script runs on each actual primary terminal creation or revival. Attaching
to a running terminal does not rerun it. Extra shells, tools, services and plain
commands bypass it, so an auxiliary shell remains available for recovery.
Repositories that want a once-per-box experience must store their own completion
state in durable user data outside the checkout. There is no automatic success
marker or automatic retry.

The script runs inside the Linux box whether your host uses macOS, Linux or
Windows. Native non-Linux boxes do not run this entry point.
