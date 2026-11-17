# 26-11-17-675 — The CLI ships a skill for the agent on the user's machine, and installs it when asked

- **Status**: Proposed
- **Date**: 2026-11-17
- **Relates to**: [0125](0125-tools-are-declared-in-files-the-way-services-are.md),
  [26-11-03-982](26-11-03-982-a-host-tool-is-handed-the-cli-server-and-project-to-set-the-discobox-up.md),
  [26-10-09-395](26-10-09-395-a-sandbox-may-be-created-with-skills.md),
  [26-10-14-294](26-10-14-294-a-skills-repository-is-a-source-its-skills-are-linked-from.md),
  [0080](0080-the-image-ships-the-skills-for-what-it-installs.md).
- **Issue**: #493

## Context

Every skill discobox ships today is for the agent **inside** a box: the
image's `discobox`, `discobox-monitor`, `discobox-access` and
`discobox-review` (ADR 0080, `sandbox-agent/features/*/skills`). Nothing
teaches the agent on the user's own machine, the one beside the `discobox`
CLI, how to use discobox from outside.

The first thing users ask that agent for is a user tool: "make me a discobox
tool for lazygit". ADR 26-11-03-982 §3 made `discobox tools --help` the
reference for that (front matter, the host environment, a worked example,
the traps a first attempt hits), and a fresh agent given only the help wrote
a working tool. What the help cannot do is make an agent *reach for it*: an
agent that does not know discobox exists, or that a tool is a file in
`<user config dir>/discobox/tools`, never runs `discobox tools --help`. A
skill's description is what puts discobox in front of the agent when the user
names it.

Two things decide the design:

- **Version.** The skill talks about commands, flags and an environment that
  belong to one CLI release. A skill that names a command the installed CLI
  lacks, or misses one it has, sends the agent wrong.
- **It must never reach a box.** `--user-skills` copies `~/.claude/skills`
  and `~/.agents/skills` into a box (ADR 26-10-09-395 §4,
  `cli/internal/sandboxcreate/skills.go`), and a copied skill wins on a name
  over the image's (§3). A host skill in a box teaches the box's agent about a
  machine it is not on, and one named `discobox` would replace the box's own
  `discobox` skill.

The CLI already writes into the user's dotfiles in one place: the `Include`
lines `discobox admin ssh-config` adds to `~/.ssh/config`, which
`discobox admin uninstall` takes back (`cli/internal/cli/ssh_config_write.go`,
`uninstall.go`).

## Decision

### 1. The skill is embedded in the CLI

The skill, `discobox-host`, is source in this repository beside the CLI and
embedded in the binary the way the builtin tools are
(`cli/internal/cli/tool_catalog.go`, `//go:embed builtin-tools`). The skill a
CLI installs is the one written for that CLI's commands, and it is reviewed in
the same change as a command it describes.

The name is not `discobox`, and not any name the image's skills use, so that
no layer of a box's skill install can mistake one for the other even if §4's
marker were lost.

### 2. It holds procedure, and points to the help for reference

ADR 26-11-03-982 §3 stands: `discobox tools --help` is the reference, and
the skill does not copy it. The skill says:

- what discobox is, seen from this machine: boxes, the CLI, a box being
  Linux whatever this machine is;
- when to use it (its `description` names "discobox" and "discobox tool");
- for a user tool, the procedure: read `discobox tools --help`; write the
  file in the directory it prints; check it with `discobox tools ls`; run it
  with `discobox tools <id> <box>` against a box made for the test, and
  remove that box after; and the judgement the help leaves to the agent
  (prefer `runs: host` when the tool carries the user's own config in;
  never copy a program from this machine, ADR 26-11-03-982 §2).

Field lists, environment variables, flags and gotchas stay in `--help`, where
they move with the code. A CLI test reads the embedded skill and fails when a
`discobox …` command line it names does not resolve to a command in the
CLI's own command tree, so a rename breaks the build, not the agent.

The first version covers user tools. The rest the issue lists (`run`, `ls`,
`exec`, `cp`, `apply`, harness configuration) are sections added later, each
under the same rule.

### 3. `discobox admin agent-skills install` puts it on this machine, when asked

A new command group, beside `admin skills` (which manages the skills *boxes*
get) and named apart from it:

- **`discobox admin agent-skills install`** writes the skill into
  `~/.claude/skills/discobox-host` and `~/.agents/skills/discobox-host`, the
  two directories `--user-skills` reads and a box installs into, under the
  home of the user running the CLI. `--dir DIR` (repeatable) names other
  skill directories instead, for a harness that reads elsewhere.
- **`discobox admin agent-skills rm`** removes what `install` wrote.
- **`discobox admin agent-skills ls`** shows where it is installed, and from
  which CLI version.

Nothing installs it unasked: not the install scripts, not the first run of
the CLI. A user's agent directories are theirs. Writing a skill there changes
what their agent does in every repository, so the user asks for it, as they
ask for `--user-skills` and for `ssh-config`.

`install` writes the whole skill directory again, so a file a newer version
dropped is gone. It refuses to write over a `discobox-host` directory it did
not write (§4's marker absent), and says so. `rm` removes only a directory
carrying the marker. `discobox admin uninstall` lists the installed copies in
its plan and removes them with everything else.

### 4. The installed skill is marked, and kept out of boxes

The installed `SKILL.md` carries, in its front matter:

```yaml
metadata:
  discobox:
    scope: host
    version: <the CLI version that wrote it>
```

`scope: host` means *not in a box*. The CLI skips a skill so marked wherever it
reads skills into a create: `--user-skills`, a `--skills DIR`, and
`client.yaml`'s `run.skills` directories. It says which it skipped, once,
rather than failing the create. The marker is in the file, not the path, so it
holds when the user copies or links the skill into some other directory. A
user who wants this skill in a box has no reason to: the box's agent is not on
this machine.

### 5. A stale copy is noticed by the agent, not refreshed behind the user

The installed copy is a snapshot of one CLI's skill, and a CLI upgrade does
not rewrite it. The skill's first instruction is to compare its `version`
with `discobox version`'s, and on a mismatch to run
`discobox admin agent-skills install` before going on. The cost of a stale
copy is small anyway: it points to the help, which is always the running
CLI's.

## Alternatives rejected

- **A skills source** (discobox-ai/skills or a new repository), installed by
  the user's own tooling (`npx skills add`, a harness's plugin marketplace).
  No CLI change, but the skill and the installed CLI drift apart with nothing
  to tell either which version the other is. Every harness installs skills its
  own way, so the instructions would be per harness. discobox-ai/skills is
  also this repository's development skills (`AGENTS.md`), for another
  audience. A project's skills sources (ADR 26-10-14-894) reach boxes only,
  never this machine.
- **The help alone, no skill.** The help is the reference once an agent knows
  to look. It does not make an agent that has never heard of discobox look.
- **The CLI installs or refreshes the skill on its own** (at install time, on
  first run, or on any run whose version differs from the installed copy's).
  It keeps the copy current, but it writes into the user's agent directories
  without being asked, and every run would check them. §5 gets the same
  result from the one moment it matters: an agent reading the skill.
- **Link the agent directories to a copy in the CLI's data directory**, which
  the CLI rewrites when its version changes. It refreshes silently, but a
  Windows link needs a junction or developer mode, `--user-skills` follows
  links (ADR 26-10-09-395 §4) so §4's marker is still needed, and a
  half-written swap is what the agent reads.
- **Keep it out of boxes by its name alone.** A list of reserved names in
  `readSkillsDir` misses a user's renamed copy. The name stays distinct (§1)
  as a second guard, not the only one.
- **An MCP server the CLI runs for the user's agent.** A running process and
  per-harness registration for what is text and a CLI the agent can already
  call.
- **Name it `discobox`.** It reads naturally on the user's machine, but it is
  the name of the box's own skill, which a stray copy would replace (§3 of
  ADR 26-10-09-395).

## Open

- **WSL.** When the CLI runs in WSL and the agent on Windows, `install`
  writes the WSL home's directories, which the Windows agent does not read.
  Revisit when someone asks; ADR 0074's Windows-side `ssh-config` is the
  precedent for reaching across.
- **Whether the install scripts and `discobox --help` mention the command.**
  A line in the install scripts' closing message is likely; left to
  implementation, provided it only says the command and never runs it.
- **The exact `description` text**, which decides when a harness loads the
  skill, is tuned in implementation against a fresh agent, as the help was.

## Consequences

- The CLI embeds a skill and gains `admin agent-skills install|rm|ls`;
  `admin uninstall`'s plan gains the installed copies.
- `sandboxcreate.ReadSkills` reads each skill's front matter and skips one
  marked `scope: host`.
- A CLI test ties every command the skill names to the command tree.
- `cli/DESIGN.md` describes the embedded skill and the marker when this lands.
