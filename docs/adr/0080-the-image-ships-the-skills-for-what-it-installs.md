# 0080 — The image ships the skills for what it installs

- **Status**: Accepted
- **Date**: 2026-09-01

## Context

[ADR 0031](0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md)
gave an agent a way to ask a person for a credential it was not provisioned
with, and `discobox-access` is installed in every sandbox. Nothing tells the
agent it is there. A model that meets a `401` reaches for what it has been
trained on — it edits a `.npmrc`, exports a variable it invents, or stops and
asks the user to paste a token into the chat, which is the one thing the whole
protocol exists to avoid.

[ADR 0072](0072-a-repository-ships-skills-that-only-exist-in-a-sandbox.md) built
the delivery half: `.discobox/skills` in the primary source is copied into
`~/.claude/skills` and `~/.agents/skills` on the primary terminal's first
launch. It is the wrong scope for this one. `discobox-access` is not a property
of the repository being worked on; it is in the sandbox whatever the sandbox was
made from, so a skill about it that only reached repositories which declared it
would reach almost nobody — including every repository whose author has never
heard of discobox, which is most of the ones a discobox is made for.

## Decision

### 1. The image installs skills, at `/usr/local/share/discobox/skills`

The sandbox image ships the skills for the things the sandbox image installs,
into a directory beside the binaries they describe. `discobox-access` is the
first; there is nothing special about it, and a second built-in skill is another
directory copied to the same place.

### 2. The sandbox agent installs them beside the repository's

The same first-launch install as ADR 0072 §2 now copies two trees into the
harness's skill directories: the image's, then the repository's. The reasons for
the placement are unchanged — the copies belong to the harness once they land,
so re-copying on every launch would restore what the agent deliberately deleted,
and only that launch is sequenced behind source delivery.

The repository's copy runs second, so **a repository declaring a skill of the
same name wins**. It is the more specific declaration, and it is the same
precedence ADR 0072 already gives a repository over an image-installed skill.

An absent built-in directory installs nothing. That is not a misconfiguration:
it is a sandbox on an image built before this, and failing its launch over a
missing skill would take away the sandbox to protect the documentation. A
directory that exists and cannot be copied still fails the launch, as ADR 0072
§3 has it — at that point something is wrong with the image rather than absent
from it.

### 3. A skill lives beside the thing it documents

`discobox-access`'s skill is `access/skills/`, in the module whose interface it
describes, and the Dockerfile copies it into the image. A skill is documentation
of an interface: it goes stale the moment a flag changes, and the only reliable
defence is that it sits where somebody changing that flag is already looking.

It also travels. `access` is a module that is meant to be liftable into another
repository ([its DESIGN](../../access/DESIGN.md)), and the instructions for
driving the CLI are part of what would leave with it — while the Dockerfile
line, the install path and the harness directories, which are all discobox's,
stay behind.

## Alternatives rejected

**Embed the built-in skills in the sandbox agent** (`go:embed`, installed by the
same code that copies them). Fewer moving parts, and no image path to agree on.
Rejected because it puts a Markdown file describing another module's CLI inside
a third module's binary: the skill would be two modules away from the flags it
documents, and updating `discobox-access` would not put anyone within sight of
it. The image already assembles what a sandbox has from every module; skills are
that same assembly.

**Put them under `sandbox-agent/image/skills/`**, beside the other files the
image installs. This is the established location for image content and would be
the right one for a skill about the sandbox itself. Rejected for this skill by
§3: nothing under `sandbox-agent/image/` documents an interface owned by another
module, and the coupling that matters here is to the CLI's flags, not to the
image's layout. A future built-in skill about the sandbox's own behavior belongs
there, and the Dockerfile can copy from both.

**Say it in the harness's system prompt or a generated `AGENTS.md`.** It would
reach the model without any install at all. Rejected because it is paid for on
every turn of every conversation in every sandbox, whether or not a credential
is ever wanted, and because the skill directories are where the harnesses
already look for exactly this — a capability described once, loaded when it
becomes relevant.

**Leave it to each repository's `.discobox/skills`.** The mechanism already
exists and needs nothing new. Rejected as the wrong scope: it asks every
repository to document a tool it does not ship, and the repositories most likely
to need the credential flow are the ones that have never declared a discobox
skill.
