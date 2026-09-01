# Discobox

**Give your agent its own disco.**
A whole computer it can do anything to — and no way to reach your keys.

Discobox runs coding agents in disposable, network-isolated boxes on your own
machine, where the agent has a complete computer — root, package installs,
whatever it wants — and none of your credentials. Everything inside the box is
unconstrained. Everything that crosses the boundary goes through one door you
control.

> **Status: in development.** The isolation, the network boundary, and the
> credential broker run today. The policy layer that evaluates data flow is
> partly built. Every claim below is tagged **Shipped**, **Partial**, or
> **Designed** — see [Status](#status).

---

## What's a discobox

A **discobox** is one disposable machine with your source already in it,
running the agent of your choice. It launches in seconds, you reach it over
ssh, git, and HTTP as if you were working inside it, and the work comes back as
commits.

**Claude Code** and **Codex** ship built in, with more to come. A harness is
just a container image carrying a label, so any terminal-based coding agent you
can install becomes one.

```bash
brew install discobox-ai/tap/discobox

cd ~/src/my-project
discobox run "fix the flaky test in the payments suite"
```

That creates a box from the current directory, starts your configured agent in
it, and streams the work back. When it's done:

```bash
discobox ls              # boxes started from this directory
discobox apply           # cherry-pick the box's commits onto your working tree
```

Typing `discobox` on its own opens the launcher instead: the same three steps —
start a box, work in it, apply what it committed — in one window.

The box runs on your machine and reaches nothing on it. Nothing was installed
outside the box, nothing ran outside it, and no credential of yours entered it.

---

## Why

Today you either lock an agent down until it can't do real work, or hand it a
machine that has your SSH keys, your cloud credentials, and your `.env` files.
Discobox rejects that trade: the constraint belongs on what an agent can
*reach*, which is finite, not on what it can *do*, which isn't.

The long version, with the diagram, is at
**[discobox.ai](https://discobox.ai)**.

---

## Security model

Six properties, each answering the doubt the previous one creates.

| | | |
|---|---|---|
| 1 | **The agent gets a whole computer.** Root, package installs, nested Docker. No tool allowlists, no command approvals. | Shipped |
| 2 | **It's isolated by VM and container.** libkrun microVMs on Linux, Virtualization.framework on macOS, user namespaces by default. | Shipped |
| 3 | **There is one way out.** All traffic leaves through a MITM proxy with a per-box mTLS identity. Destination policy, and every request audited. | Shipped |
| 4 | **Credentials never enter the box in cleartext.** The agent holds sentinels; the proxy swaps in the real value on the way out, bound to one domain. | Shipped |
| 5 | **Privileged use is granted by a human.** The agent asks and says why; you approve; the grant carries a host, a scope, and an expiry. | Shipped |
| 6 | **Policy is written in English.** Grants are natural language evaluated by a model, so one grant covers a class of requests rather than one call. | Partial |

The payoff of (4) is worth stating directly: **prompt injection can't steal
what isn't there.** A fully compromised agent — running an attacker's
instructions, with root — still has no credential to exfiltrate and still has
to get past the door.

**[Read the full security model →](https://discobox.ai/security)** — the
threat model, the asset matrix including its weak cells, and what Discobox does
not defend against.

### Known limitation

The check that a command matches its approved use currently runs *inside* the
sandbox, on a description the agent supplies. An agent that misrepresents what
it is about to run can obtain a sentinel for a use you didn't approve.

What still holds, because it's enforced outside the box: the credential never
enters the sandbox, the sentinel only resolves against the granted domain, a
human approved that grant, and every injection is audited. The gap narrows the
guarantee from *"this specific use was approved"* to *"this credential was
approved for this domain"* — it does not expose the credential.

So this layer is a **guardrail, not a security boundary**: it defends against
an agent that errs, not an agent that deceives.
[ADR 0031](docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)
specifies the fix — verification on the trusted side, enforced against observed
outbound traffic rather than a declared command. Designed, not implemented.

---

## What it's like to use

The reason you work in your own checkout is that everything is already there.
A sealed box gives that up. Most of Discobox is spent giving it back.

- **Reachability** — `ssh $DISCOBOX_ID` works after
  `discobox admin ssh-config --write`. git, HTTP, and ssh are proxied through
  the CLI. HTTP services in the box are detected and forwarded to local ports
  with `discobox proxy`.
- **Tool parity** — direnv is wired up, so nix, mise, or whatever your project
  already declares pulls your toolchain into the box on entry.
- **Code flow** — source is cloned in; commits come back out with
  `discobox apply`. Work leaves as commits, not a diff you have to trust.
- **Programmability** — a full OpenAPI surface and a scriptable CLI. Anything
  the TUI does, a shell script can do.

---

## Concepts

Three primitives, and no fourth.

| | | |
|---|---|---|
| **Discobox** | *the box* | A disposable environment with your source in it. One default terminal — the harness, or a shell — and its own git remote. |
| **Pool** | *the floor* | The host boxes are scheduled onto and the boundary they share: a cache volume, a resource envelope, and a kernel. |
| **Harness** | *the act* | The agent a box runs. Claude Code and Codex built in; any terminal agent you can put in an image becomes one. Configure once; every box gets it. The built-in shell is a harness too. |

---

## Commands

```
discobox            Open the launcher
discobox run        Launch a prompt in a new discobox
discobox ls         List discoboxes started from this directory
discobox shell      Run a command in a box, or open its login shell
discobox attach     Attach to a box's terminal
discobox apply      Cherry-pick a box's commits onto your working tree
discobox push       Push local commits into a box's origin, to rebase there
discobox proxy      Forward a box's listening ports to local ports
discobox cp         Copy files in and out
discobox tools      Run git, ssh, or VS Code against a box
discobox secret     Manage secrets, grants, and approval requests
discobox configure  Enable, disable, and set the default harness
discobox tui        Launch the interactive discobox launcher
discobox admin      Pools, projects, harness images, and the API server
```

`discobox --help` for the rest.

---

## Status

Pre-1.0 and under active development. Interfaces change.

| Area | |
|---|---|
| Discoboxes, pools, harnesses | Shipped |
| Claude Code and Codex harnesses, plus the built-in shell | Shipped |
| Bring-your-own agent as a registered harness image | Shipped |
| VM and container isolation | Shipped |
| MITM proxy, per-box mTLS identity, destination policy, audit | Shipped |
| Sentinel credential injection, domain binding, human approval | Shipped |
| ssh / git / HTTP access, port forwarding, direnv, apply | Shipped |
| Natural-language grants evaluated by a model | Partial — see [above](#known-limitation) |
| Trusted-side verification of credential use ([ADR 0031](docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)) | Designed |
| Policy compiled from English to a deterministic engine | Designed |
| Response-side inspection and data-flow policy | Designed |
| Remote pools — boxes hosted somewhere other than this machine | Designed |

Every box runs on your own machine today. Pools are the seam where that stops
being true, and hosting them elsewhere is planned, not built.

Platforms: macOS, Linux, Windows.

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
