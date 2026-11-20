# Shell harness

The login shell, and the end of the harness resolution chain. Its declaration
is [`harness.json`](harness.json); what every built-in shares is
[`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).

| Identity | |
| --- | --- |
| Directory, harness ID and built-in config slug | `shell` (`harness.ShellSlug`, reserved) |
| Bundle | `discobox-harness-shell` |

It declares every part absent, with a reason: the platform already ships the
shell, so there is no install; its reserved slug withholds the harness-run
convention, since a command typed into this terminal would be a second shell
inside the first (ADR 0043, ADR 0086 §3), so there is no launcher; and there
is no agent to trust a folder, remember anything, sign in, publish hooks or
save anything back. Its env and volumes are the platform's layer.

Its bundle is its label alone, built by `harness/Dockerfile`'s `label` stage:
no feature, and so no layer, which is what the shell's bundle has always been.
Its adapter is the empty one the shape requires, and its bundle does not
carry it.
