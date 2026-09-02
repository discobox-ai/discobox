# 0086 — A harness image extends the sandbox base, and its manifest is override-only

- **Status**: Accepted
- **Date**: 2026-09-02
- **Supersedes**: [0043](0043-shell-is-an-ordinary-harness-image.md) §2 — an omitted `runCommand` meaning the sandbox's login shell. §1 (`shell` is an ordinary harness image) and §3 (`Configured` is derived from what the image collects) stand unchanged.

## Context

A sandbox image declares itself in one OCI label, `io.discobox.image.v1`,
authored as an `image.json` beside its `Dockerfile` and compacted into a build
argument (ADR 0012 §6 removed the runtime file; the label is the only carrier).
The label is a *complete* manifest: `env`, `volumes`, `additionalGroups`, and
the `harness` contract.

Complete, and therefore duplicated. Every harness image is built `FROM` the
sandbox agent base, and the base's own facts — the `NIX_*`/`PATH`/
`NPM_CONFIG_PREFIX` block, `DISPLAY=:0` for the socket-activated desktop, the
fourteen `data`/`cache` volumes, the `docker` supplementary group, the pnpm
`storeDir` seed file — are true of the base and restated by hand in each leaf.
Of `harness/shell/image.json`'s 39 lines, 28 are that restatement; the file's
own content is an id, a name, and a description. `harness/DESIGN.md` has
carried the admission since ADR 0043: *"every base-provided fact is restated by
hand until there is a composition pattern for these manifests."*

Restating by hand fails in both directions, and both failures are live:

- `test/harness-stub/image.json` and
  `test/performance/terminal-latency/image/image.json` declare no `env`,
  `volumes`, or groups at all. They are built `FROM` the same base and get none
  of its wiring — not a decision, an omission that reads as one.
- `discobox harness register --image` is a first-class flow. Someone building
  their own harness image has no way to obtain those facts except to copy our
  JSON, and no way to learn that it changed. Since ADR 0043 the copied block has
  grown twice: three `/nix` volumes, then `DISPLAY`.

The rest of the manifest is a second problem wearing the same clothes. What
`runCommand` and `relaunchCommand` say, the runtime has already fixed:

- A primary terminal's first launch appends the sandbox's prompt to the harness
  command as arguments (`terminal/service.go`, `primaryCreateRequest` →
  `command = append(command, req.Args...)`).
- A relaunch swaps in the second command (`reviveStartupCommand`).

So `["claude"]` plus `["claude","--continue"]` is a per-image spelling of a
shape every image has. The image is not declaring behavior; it is naming a
binary, twice, in a structure the runtime imposed anyway.

And one question sits behind all of it: could an arbitrary image — `ubuntu` —
be a harness image? No, and not for any reason a manifest could fix. PID 1 must
be `discobox-sandbox-agent init`; every exec is a systemd transient unit
(`execs/systemd.go`, `systemd-run`); nested Docker trust needs the
`discobox-runc` wrapper (ADR 0020). The runtime contract lives in the image's
filesystem, not in its metadata. An image that does not extend the sandbox base
cannot run a sandbox, whatever it declares.

## Decision

### 1. Extending `discobox-sandbox-agent` is required, and the base layer proves it

A harness image is built `FROM` the sandbox agent base. This was already true of
every image that works; it is now a stated requirement with a check behind it,
and the requirement is what makes everything below safe to assume rather than
merely likely.

The proof is the base's own manifest layer (§2). An image carrying no such layer
did not come from the base, and registration rejects it by saying so — *this
image is not built FROM discobox-sandbox-agent* — rather than by reporting a
missing label, which is the same fact phrased as a paperwork error.

### 2. A manifest is a stack of inherited layers

An image's effective manifest is the ordered merge of every layer its image
config carries:

| Label | Role |
| --- | --- |
| `io.discobox.image.v1.<NN>-<name>` | A contributed layer. Merged in ascending lexical order of the suffix. |
| `io.discobox.image.v1` | The image's own layer. Always merged last. |

Nothing about this is a new transport. Docker inherits the parent image config's
labels, and a `LABEL` instruction replaces only the key it names — the property
`io.discobox.reclaimable.v1` already relies on to survive `FROM` and a registry
round-trip (ADR 0040). A layer set by `sandbox-agent/Dockerfile` is therefore
present, unchanged and unrequested, on every image built from it, and reaches
the control plane in the single image inspection it already performs.

The count is open. `base-image → sandbox-agent → harness` is three levels today
(ADR 0068), and an image may carry one contributed layer or six, each owned by
whichever `Dockerfile` installs the software it describes. Order lives in the
key rather than the payload so a layer's position is fixed by its author and
readable without parsing anything, the way a `*.d` directory orders its
fragments. `00`–`49` is reserved for layers Discobox ships; a downstream image
numbers from `50` so it cannot silently replace one.

Layers merge by identity, never by position: `env` per key, `additionalGroups`
by union, `volumes` by `path`, `harness.files` by `path`, `harness.secrets` by
`name`, and `harness` scalars by last-non-empty. A later layer replaces an entry
in place and appends new ones, so a base ordering is not reshuffled by a leaf
overriding one member of it. Positional merging would make "the third volume"
load-bearing between files written by people who have never seen each other's.

There is no unset. A leaf overrides any inherited entry and deletes none, which
is a restriction on what belongs in a shared layer rather than machinery: a
layer states what its own image installed.

Validation moves to after the merge. A layer with no `harness` block, no id, and
no name is legal — that is what a base layer *is* — and the rules that make an
image registrable are applied once, to the merged result.

This is not the base image becoming something the harness-config seeder reads.
ADR 0043 rejected "give the sandbox agent base its own harness-less label"
because it would make the base simultaneously the thing harnesses are built on
and an image the seeder inspects and holds a digest for. That objection is
untouched: the base layer travels *inside the harness image's own config*, the
server inspects exactly one image, and the base image's identity is still
something no control-plane path resolves. The freshness key survives with it —
a change to the base layer changes the base image, so a harness image rebuilt on
it has a new config, inherited labels included, and a new config digest, which
is what ADR 0016's re-snapshot already compares.

### 3. The harness command is a convention

The image installs its agent as `discobox-harness-run` on `PATH`, alongside the
`discobox-sandbox-agent`, `discobox-access`, and `discobox-ca-anchor` binaries
the base already ships there. The runtime types:

```
discobox-harness-run [--resume] '<prompt>'
```

`--resume` marks a relaunch — a terminal coming back after the sandbox stopped,
or a revive in place (ADR 0038). The wrapper decides what that means for the
agent it wraps; for Claude Code it is `--continue`, for Codex `resume --last`,
and for an agent with no resume story it is ignored.

The base image ships `discobox-harness-run` as a shim that execs nothing, so an
image which installs no agent lands the user at a clean prompt rather than at a
`command not found`. An image that installs one overwrites the shim, typically
with three lines.

This works because a harness command is *typed into a login shell's PTY* rather
than executed as argv (ADR 0027). A convention that names a binary the image may
not have is safe exactly here: the shell reports it and hands back a live
prompt, so the failure mode of a wrong guess is a working terminal.

`runCommand` and `relaunchCommand` remain in the manifest as **overrides**, for
an image whose agent cannot be wrapped or that wants a different second command.
Nothing Discobox ships sets them.

The `shell` harness needs no exception. Typing is already gated on the reserved
harness id (`harnessID != ShellHarnessID`), so the shell terminal types nothing
whatever the convention would otherwise supply.

### 4. Every launch carries the prompt, including a relaunch

The sandbox's prompt is appended to the harness command on *every* launch, not
only the first. Today a relaunch drops it: `reviveStartupCommand` returns the
relaunch command alone, and `primaryCreateRequest`'s relaunch branch sets
`req.command`, which the assembly path takes in preference to `req.Args`.

Passing it always is a deliberate redundancy, and the reason is the failed case
rather than the working one. A harness that resumes a session does not need the
prompt — but a sandbox whose first launch failed has no session to resume, and
its user arrives at a terminal that has forgotten what it was for. Because the
command is typed, the prompt is *on screen*, in the scrollback, as an editable
command line: the user sees what the sandbox was asked to do and can press
enter. A wrapper that has a session to resume ignores the argument, which costs
nothing.

Prompts are rendered through `execs.QuoteShellArg`, whose POSIX single-quoting
already handles quotes and newlines, so arbitrary prompt text on a typed command
line is not a new hazard.

### 5. Identity defaults to the registration; credentials stay declared

`harness.id` and `harness.name` become optional. `CreateHarnessConfig` already
prefers the caller's `--slug`/`--name` and falls back to the label only when
they are absent; it now tolerates a manifest that supplies neither, failing only
when the registration names nothing either.

`secrets` and `config` stay declarative, and they stay together. Nothing can
infer that an agent wants `ANTHROPIC_API_KEY`, and a convention cannot
distinguish an image with no configure flow from an image that forgot one —
both are questions about credentials, which is the one area where the image has
something to say that its filesystem does not already say. An image that needs
neither declares nothing at all.

## Consequences

- `sandbox-agent/image.json` is new and is the base layer: the `NIX_*`/`PATH`/
  `NPM_CONFIG_PREFIX` env, `DISPLAY`, all fourteen volumes, the `docker` group,
  and the pnpm `storeDir` seed file. The three harness manifests keep only what
  is theirs, and `harness/shell/image.json` stops existing — its id is the
  reserved slug, its name and description are the registration's.
- An image that installs an agent as `discobox-harness-run` and needs no
  credentials carries **no manifest at all**. Its `Dockerfile` is `FROM
  discobox-sandbox-agent`, an install, and a rename.
- The two test-only harness images inherit the base wiring they never declared.
  This changes their behavior, and correctly: they were built on the base all
  along.
- Registering an image not built on the base fails with that sentence, where it
  previously failed with a missing-label error or, worse, registered and
  produced a sandbox that could not boot.
- Version skew is one-directional: a slim, layered harness image registered
  against a server predating this ADR resolves to its leaf layer alone, losing
  env and volumes. Images and server ship together per release.
- Layer *names* are the collision surface. Two images in one chain using the
  same key produce one layer, the descendant's, with no diagnostic — the same
  failure a `*.d` directory has with two identically-named fragments, and the
  reason for the reserved range.
- `harness.ResolveImageLabels` is the one implementation of the stack, in the
  root module, used by the server's label path and by the dev-manifest path.
  Nothing merges manifests at build time, so `Taskfile.yml` keeps compacting
  each `image.json` independently with `jq`.

## Alternatives considered

**Two ADRs: layering, then conventions.** They look separable — one is about
label transport, one about a binary name. Rejected because the convention is
only safe *because* extending the base is required, and requiring that is only
enforceable *because* the base contributes an inherited layer to check for. Split
across two decisions, each half reads as an unjustified assumption about the
other.

**Support an arbitrary image (`ubuntu`) by deriving one.** On registration,
build `FROM ubuntu` plus the runtime layer on the pool's shared BuildKit
(ADR 0044), resolved through the per-sandbox registry namespace (ADR 0047).
Both mechanisms exist. Rejected for now, and worth naming as deferred rather
than dismissed: it is a real feature with a real cost — a build, a push, and a
cache to invalidate on every registration — and it changes nothing about this
ADR, which is what an image must contain once it exists. Revisit when someone
wants a base image Discobox does not publish.

**Merge at build time via an `extends:` chain in the repository.** Each
`image.json` names its parents by path and a resolver flattens them before the
label is written, so the published label stays a single complete manifest and
neither the server nor the label format changes. Rejected because composition
would stop at our own build: the label a third-party image needs is the one
thing the mechanism cannot give it, and `harness register --image` is the case
the duplication hurts most. It also adds a build-time resolution step to three
producers where inheritance adds none.

**An `image.json.d/` directory per harness.** Splits the file and orders
fragments the same way this ADR's keys do. Rejected because it solves the wrong
half: the shared fragment is still copied or symlinked into each harness
directory, and an image built `FROM` the base inherits nothing.

**A fixed two-level base/leaf split**, one `io.discobox.image.base.v1` key and
one leaf key. Enough for the duplication that exists today. Rejected because the
depth is not ours to fix — the chain is already three levels — and ordering by
key costs one convention while removing the ceiling.

**Declare the shared facts in Go, as control-plane defaults.** Now that
extending the base is required, the server could simply know what the base
declares. Rejected for the reason ADR 0043 rejected it for `additionalGroups`:
image-owned data would live in the control plane, and the image digest would
stop being a complete freshness key for what the image declares. A base image
and a server would then be able to disagree, with no way to notice.

**Keep `runCommand` declarative and only inherit the data fields.** Rejected
because it preserves the least defensible half. `["claude"]` and
`["claude","--continue"]` name a binary twice in a shape the runtime already
fixed, and a manifest that exists solely to say which binary is a file that
exists to be forgotten when someone renames one.
