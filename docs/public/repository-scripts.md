# Repository startup scripts

On Linux boxes, a repository may provide an executable
`.discobox/scripts/pre-harness`. Discobox runs it without asking, in the
primary terminal, before the selected harness starts, so the script can
interact with you. It runs as the box user, with the primary terminal's
environment, credential bindings, working directory and terminal. Read the
repository's code before you run a source you do not trust.

Only the primary source supplies the script. Its path is relative to that
checkout's root, even when commands start in a subdirectory. It must be a
regular executable file with a valid interpreter. A symlink must resolve
inside the checkout.

Exit zero to continue to the selected harness. Any failure, an invalid
executable or an interrupt stops the harness from launching. The login shell
stays open so you can repair the script. With the shell harness, success
returns to that login shell. The script receives no harness prompt and no
resume arguments.

The script runs each time the primary terminal is created or revived. Attaching
to a running terminal does not run it again. Extra shells, tools, services and
plain commands skip it, so an extra shell stays available for recovery.
Discobox keeps no success marker and does not retry a failed script on its own.
A repository that wants the script to run once per box must store its own
completion state in durable user data outside the checkout.

The script runs inside the Linux box whether your host runs macOS, Linux or
Windows. Native boxes on other operating systems do not run it.
