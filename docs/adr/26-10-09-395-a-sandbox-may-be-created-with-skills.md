# 26-10-09-395 — A sandbox may be created with skills

- **Status**: Proposed
- **Date**: 2026-10-09
- **Relates to**: [0072](0072-a-repository-ships-skills-that-only-exist-in-a-sandbox.md),
  [0080](0080-the-image-ships-the-skills-for-what-it-installs.md),
  [0012](0012-sandbox-config-is-three-attribute-owned-layers.md),
  [26-10-08-127](26-10-08-127-a-sandboxs-bootstrap-is-static-and-the-intake-carries-the-rest.md).

## Context

A sandbox's harness gets skills from two places today: the image's
(`/usr/local/share/discobox/skills`, ADR 0080) and the primary source's
`.discobox/skills` (ADR 0072). Both are copied into `~/.claude/skills` and
`~/.agents/skills` on the primary terminal's first launch.

Neither covers the skills belonging to the *person creating the sandbox*: the
ones in their own `~/.claude/skills` or `~/.agents/skills`, or a folder of
skills they keep for a kind of task. Those are not the repository's to declare
and not the image's to ship, and nothing in the sandbox can read them, since
nothing on the client is reachable from inside it.

## Decision

### 1. The create request carries skills as content

`SandboxCreateConfig` gains `skills`, a map from skill name to the skill:

```json
"skills": {
  "foo": {
    "skill": "<the SKILL.md text>",
    "files": [
      {"path": "scripts/run.sh", "content": "<base64>", "executable": true},
      {"path": "reference/notes.md", "content": "<base64>"}
    ]
  }
}
```

- A name is one path segment: not empty, no `/` or `\`, not `.` or `..`, and
  not hidden.
- `skill` is required and is what lands at `<name>/SKILL.md`.
- Each file's `path` is relative and clean, stays inside the skill, and is not
  `SKILL.md`. `content` is bytes, base64 on the wire, so a skill's images and
  binaries survive. `executable` carries the one permission a skill needs
  (ADR 0072 §1).
- The server rejects a request whose decoded skills total more than 1 MiB.
  `sandbox.json` is a bootstrap every backend has to place before boot (§2), and
  skills are the first thing in it that has no natural size.

The request carries content, not paths, because the server and the pool cannot
read the client's disk. Reading a directory is the CLI's job (§4).

### 2. Skills are create-time spec, carried in the bootstrap

The server persists them in `SandboxManifest`, in a JSON column that is omitted
when empty. A sandbox created without skills therefore keeps the manifest
fingerprint it had before this change, and no existing container is rebuilt.
Skills are fixed at create, like `prompt`, `git` and `description`.

The pool create request carries them to the pool. The pool-agent writes them
into `sandboxconfig.RuntimeLayer.Skills`, and `Effective` copies them to
`Config.Skills` (single writer). The sandbox reads them from `sandbox.json`.
That is ADR 26-10-08-127 §1's static half: the skills hold no secret, and they
do not change while the sandbox exists.

Reads of a sandbox report the skill names and not their content, so listings do
not grow with every skill attached to every sandbox.

### 3. They are the third layer of the first-launch install, and win on a name

`installSkills` copies, in this order: the image's skills, then the primary
source's `.discobox/skills`, then the request's skills. Each goes into both
skill directories, once, on the primary terminal's first launch. That is the
same place and the same once-only rule as ADR 0072 §2. After the copy they are
the harness's files; a restart never restores one the harness pruned or
edited.

The request's skills come last because they are the most specific declaration.
The person creating this sandbox asked for them, for this sandbox. The overlay
semantics stay the same: overwrite by file, never delete what was already
there.

### 4. The CLI reads skill directories into the request

`discobox new` (and `sandbox create`) gain:

- `--skills DIR`, repeatable. Every immediate subdirectory of `DIR` that holds a
  `SKILL.md` is one skill, named for the subdirectory.
- `--user-skills`, opt-in. It reads `~/.claude/skills` and then
  `~/.agents/skills` the same way, skipping either one that is absent. Nothing
  from the home directory leaves the machine unless asked for.

The sources are read in this order: `--user-skills` (`~/.claude/skills`, then
`~/.agents/skills`), then each `--skills` in the order given. A later
declaration of the same name replaces an earlier one whole, so an explicit
`--skills` always beats the home directory. A subdirectory without `SKILL.md` is
not a skill and is skipped.

The CLI follows symbolic links, since entries in `~/.claude/skills` are often
links into a checkout. It reads content, so a link resolves on the client where
it means something, unlike ADR 0072's copy inside the sandbox. A symlinked
directory below a skill's root is skipped so that a link cannot loop, and
`.git` is skipped. The CLI checks the 1 MiB limit before it sends the request
and names the largest skills when that check fails.

## Alternatives rejected

- **Deliver skills in the runtime-config document (the intake).** The document
  exists for what changes while a sandbox runs, and it is re-sent whole on every
  change (ADR 26-10-08-127 §1). Skills never change, so carrying them there
  would re-send them with every secret rotation, for no gain.
- **Express skills as `Files` entries at `~/.claude/skills/...`.** That needs
  no new field, but `Files` are installed before every terminal, which restores
  what the harness pruned (ADR 0072 §2 rejects exactly that), and its paths
  would have to name a home directory that only the sandbox can resolve.
- **Have the CLI `cp` the skills in after create.** That needs no API change,
  but a sandbox created through the API alone could not have skills. It would
  also race the first harness launch, which reads skills when it starts.
- **Send paths and have the server read them.** The server is often not on the
  client's machine, and it never reads a client's disk.
- **Install request skills on every start.** That restores a skill the harness
  deliberately removed, and it would make a request skill behave differently
  from the image's and the repository's.
- **Error on a duplicate skill name across sources.** Under that rule, adding
  `--user-skills` to a command that already works could break it. A deliberate
  order where the explicit flag wins is more useful and easy to predict.

## Consequences

- `SandboxCreateConfig` gains `skills` (request) and the sandbox response
  gains the skill names; the pool create request and `sandboxconfig` gain the
  same field.
- `sandbox.json` can grow by up to 1 MiB. A backend that places it through
  something with a smaller limit (cloud user-data) has to place it some other
  way.
- `sandbox-agent/terminal/skills.go` installs from three sources instead of
  two; its `DESIGN.md` describes the order.
