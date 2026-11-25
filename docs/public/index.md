# Discobox manual

Discobox is a harness runtime. Give it a harness, a source and a task. Discobox
runs the harness in a sandbox of its own, with its own copy of the code. The
agent has full control inside the sandbox and never waits for approval. Your
secrets stay outside the sandbox, and everything the agent sends leaves through
one proxy.

You can run one box per task, with several side by side. You can keep a named
agent running for months. You can add your team's skills and let a lead box
run your process as a software factory. You collect the work through Git.

- [Getting started](getting-started.md): install, configure an agent, run a task,
  and collect the work.
- [Concepts](concepts.md): boxes and sources, harnesses, skills, boxes that
  lead other boxes, providers, lifecycle, disk use and credentials.
- [Running boxes on boxd](boxd.md): set up a boxd provider, and what its key
  can reach.
- [Console guide](console.md): workflows, navigation and keybindings.
- [CLI reference](cli.md): generated public commands and flags.

This manual and the command reference live in the product's repository. A
change to a command, a default or console behavior must update these pages in
the same change. The documentation site imports and reviews snapshots of them
separately from product releases.
