# Discobox v0.19 manual

**Prerelease documentation.** This manual describes the v0.19 development
line. It does not declare v0.19 stable. Use the exact product version and
revision shown by the documentation site's snapshot manifest; a prerelease
snapshot stays labeled prerelease until a reviewed stable import replaces it.

Discobox is a harness runtime. Give it a harness, a source and a task, and
it runs the harness in its own sandbox with its own copy of the code. The
agent has full control inside the sandbox and never waits for approval. Your
secrets stay outside it, and everything it sends leaves through one proxy.

Run one box per task side by side, keep a named agent running for months, or
add your team's skills and let a lead box run your process as a software
factory. Work comes home through Git.

- [Getting started](getting-started.md): install, configure an agent, run a task,
  and collect the work.
- [Concepts](concepts.md): boxes and sources, harnesses, skills, boxes that
  lead other boxes, providers, lifecycle and credentials.
- [Console guide](console.md): workflows, navigation and keybindings.
- [CLI reference](cli.md): generated public commands and flags.

The manual and command reference are maintained alongside the product. A
change to a command, default or console behavior must update these pages in
the same change. Documentation snapshots are imported and reviewed separately
from product releases.
