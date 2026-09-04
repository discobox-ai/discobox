# Discobox

**Give your agent its own disco.**
A full computer, no supervision, dance like nobody's watching. (Oh but we are.)

```bash
brew install discobox-ai/tap/discobox
```

---

## The discobox way

**1. A new box, with your source copied into it**

```bash
cd ~/src/my-project && discobox
```

**2. Work with the agent until the change is a commit**

```bash
git commit -am "..."
```

**3. Take that commit back out to your source**

```bash
discobox apply
```

`discobox` on its own opens the launcher: every box you have, in one window.
Enter starts a new one and drops you into it — the agent's terminal, the shells
and services beside it, its forwarded ports. `Ctrl-A d` leaves; the box keeps
working. Or skip the window entirely:

```bash
discobox run "fix the flaky test in the payments suite"
```

Nothing was installed outside the box, nothing ran outside it, no credential of
yours entered it, and the work comes back as commits rather than a diff you
have to trust.

---

## Maximum autonomy, because there is a boundary

Today you either lock an agent down until it can't do real work, or hand it a
machine that has your SSH keys, your cloud credentials, and your `.env` files.
Discobox rejects that trade: the constraint belongs on what an agent can
*reach*, which is finite, not on what it can *do*, which isn't.

Inside the box nothing is in the agent's way: root, package installs, nested
Docker, `rm -rf /` if it decides to. No allowlists, no approvals, no babysitting
a permission prompt. Everything that leaves goes through one door you control —
a per-box mTLS identity through a MITM proxy, destination policy, every request
audited — and your credentials never enter at all. The agent holds sentinels;
the proxy swaps in the real value on the way out, bound to one domain. For
anything it wasn't given, it asks a human and says why.

Which means **prompt injection can't steal what isn't there.** A fully
compromised agent, running an attacker's instructions, with root, still has no
credential to exfiltrate and still has to get past the door.

**[Read the full security model →](https://discobox.ai/security)** — the threat
model, the asset matrix including its weak cells, and what Discobox does not
defend against.

---

## Remote development that's actually good

Remote development has always been a downgrade: you gave up your editor, your
shell, your toolchain, and your ports, and got latency in exchange.

Agentic development is a different shape. You're not typing in the box, you're
steering, reading, and running the thing — a workload a remote environment is
*better* at, because boxes are disposable, several run at once, and the one
you're not watching is still working. Discobox is built for that shape, and
most of it is spent giving back what a sealed box takes away.

### Getting in

- **`ssh $DISCOBOX_ID` just works**, by id or by name, with nothing to set up.
  The launcher rewrites the project's managed `ssh_config` whenever it hands you
  an address, and `~/.ssh/config` carries one Include line pointing at it — on
  Windows, for both ssh installations, this side's and the one Windows tools
  drive. `discobox admin ssh-config --write` is the catch-up for a box you made
  some other way.
- **VS Code** opens on a box in a window of its own, over Remote-SSH:
  `discobox tools vscode`.
- **The launcher** is a full TUI: your coding agent's own interface on the left,
  the shells you open on the right, services and forwarded ports beside them.
  The mouse works; `F1` lists every key.
- **Terminals revive in place.** Close the window, reattach from another
  machine, come back tomorrow — same session, same scrollback, and a durable
  transcript rather than a tab you lost.
- **`discobox shell`** runs a command or a login shell, **`discobox cp`** copies
  files in and out scp-style, **`discobox tools git`** runs git in the box's
  working tree from here.

### Your toolchain, your project

- **direnv is wired up**, so nix, mise, or whatever your project already
  declares pulls your toolchain in on entry. Nothing to re-declare.
- **The Nix store is a pool-shared cache**, seeded on first use, so the second
  box doesn't rebuild the first one's world.
- **`.discobox/services`** declares what runs beside the work — the API, a
  database, a watcher. The box starts them, names them, keeps their output.
- **`.discobox/skills`** gives the agent skills that exist only inside the box.
  Your `~/.claude/skills` on your laptop stays untouched.
- **`.discobox/sources.json`** names the sibling repositories this one is worked
  on with; they're checked out alongside it.
- **Listening ports are found for you** — a watcher reads what's bound and
  probes it, and `discobox proxy` forwards it to a stable local port.

### In the box

- **Claude Code and Codex ship built in**, and the shell is a harness too. Any
  terminal agent you can put in an image becomes one.
- **Nested Docker works**, builds included: they run on a pool-shared BuildKit
  and trust the proxy through a runc wrapper.
- **Code review with no forge involved.** `discobox-review` is a review of the
  working tree that lives in the box: comments anchored to lines, replies,
  per-file approval. Two agents run it against each other — one reviews and
  comments, the other fixes and answers — until every thread is closed and every
  file signed off. Nothing to push, no PR to open, no rebase to survive.
- **`fresh`** is in there as an editor, and both tools come from the image, so
  everyone looking at the same box is looking at the same version.
- **Git authorship is a first-class property** of the box, so commits come back
  attributed correctly.
- **The agent can ask for a credential** it wasn't given, say why, and get a
  scoped, expiring grant from a human.

### Running it

- **Boxes upgrade and repair in place**, preserving power state; deletes are
  archive-then-purge rather than a surprise.
- **Everything is scriptable.** A full OpenAPI surface and a CLI to match —
  anything the launcher does, a shell script can do.
- **macOS, Linux, and Windows.** libkrun microVMs on Linux,
  Virtualization.framework on macOS, user namespaces by default.

---

## Concepts

- **Discobox** — *the box.* A disposable environment with your source in it, one
  default terminal, and its own git remote.
- **Pool** — *the floor.* The host boxes are scheduled onto and the boundary
  they share: a cache volume, a resource envelope, a kernel.
- **Harness** — *the act.* The agent a box runs. Configure once; every box gets
  it.

---

## Commands

```
discobox            Open the launcher
discobox run        Launch a prompt in a new discobox
discobox ls         List discoboxes started from this directory
discobox shell      Run a command in a box, or open its login shell
discobox attach     Open a box: its terminal and what runs beside it
discobox apply      Cherry-pick a box's commits onto your working tree
discobox push       Push local commits into a box's origin, to rebase there
discobox proxy      Forward a box's listening ports to local ports
discobox cp         Copy files in and out
discobox tools      Run git, ssh, or VS Code against a box
discobox secret     Manage secrets, grants, and approval requests
discobox configure  Enable, disable, and set the default harness
discobox admin      Pools, projects, harness images, and the API server
```

`discobox --help` for the rest — `tui`, `completion`, and every flag. `run` can
be left off the thing you do most: `discobox fix the failing tests` is a run.

---

## Status

Pre-1.0 and under active development; interfaces change. Everything above runs
today. Three things are designed and not yet built:

- **Trusted-side verification of credential use.** The check that a command
  matches its approved use runs *inside* the box today, on a description the
  agent supplies — a guardrail against an agent that errs, not a boundary
  against one that deceives. Either way the credential never enters the box, the
  sentinel only resolves against the granted domain, a human approved the grant,
  and every use is audited.
  ([ADR 0031](docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md))
- **Policy compiled from English** to a deterministic engine, plus response-side
  data-flow inspection.
- **Remote pools.** Every box runs on your own machine today; pools are the seam
  where that stops being true.

---

## Documentation

- [discobox.ai](https://discobox.ai) — overview and
  [security model](https://discobox.ai/security)
- [DESIGN.md](DESIGN.md) — system architecture
- [docs/adr](docs/adr) — architecture decisions and the alternatives rejected

## Contributing

The toolchain comes from the Nix flake — `nix develop`, or let direnv do it.
See [CLAUDE.md](CLAUDE.md) for repository conventions and
[Taskfile.yml](Taskfile.yml) for build targets.

## License

See [LICENSE](LICENSE).
