# Shell Harness Design

The `shell` bundle: the login shell, and the end of the resolution chain. What
every harness shares is in [`harness/DESIGN.md`](../DESIGN.md).

- Its Dockerfile installs nothing (the platform already ships the shell) and
  its `image.json` names only the platform API version it runs on: no identity
  to declare beyond its reserved slug, no secrets, no save-back, and its env
  and volumes are the platform's layer. Its bundle carries no feature, only
  its label, and no `agent.conf`, so the agent version store leaves it alone.
- The reserved slug is also what withholds the harness-run convention from it
  — a command typed into this terminal would be a second shell inside the
  first. It is otherwise an ordinary harness in every mechanism that touches
  it (ADR 0043, ADR 0086 §3).
- Shell collects no credentials and declares no destination policy.
