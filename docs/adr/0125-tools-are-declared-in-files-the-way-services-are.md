# 0125 — Tools are declared in files, the way services are

- **Status**: Accepted (§4's `/` for `{workdir}` with no working tree, and the
  consequence that such a VS Code window opens `/`, superseded by
  [0141](0141-a-host-tool-with-no-working-tree-opens-the-working-root.md);
  everything else stands)
- **Date**: 2026-09-16
- **Supersedes**: [ADR 0071](0071-a-tool-session-is-an-exec-the-launcher-labeled.md)
  §6 (the catalog is the launcher's table), the part of §7 that makes a
  tool file's default a Go constant, and §11 (`{workspace}` in a file's
  destination). §1–5, §8–10 and §12 stand.
- **Extends**: [ADR 0094](0094-an-image-declares-services-in-the-format-a-repository-does.md),
  whose declaration format this gives a metadata-only form.

## Context

The tools picker and `discobox tools` offer six things, and they are built
three ways:

- `git` and `ssh` are commands with behavior of their own: git runs in a
  source's working tree, ssh carries its session over this CLI's connection.
- `diff` and `fresh` are rows in a Go table in `cli/internal/tui` with a
  command to exec in the box. They have no CLI command at all. Their
  configuration defaults are Go string constants.
- `vscode` and `zed` are an `Editor` enum, a switch in the TUI adapter, and two
  near-identical cobra commands that differ in a binary list, one environment
  variable, and how the remote is spelled on the command line.

Nothing about the last four is specific. Each is either **a program run in the
discobox**, or **a program run on this machine that is handed the discobox's
ssh host, working tree, or git URL**. But none can be added without a release:
not by the image that ships `fresh`, not by a repository whose team uses a
different reviewer, and not by a person who uses an editor nobody listed.

Services already solved the adjacent problem — declared-by-file things, from
the image and from the repository, in one format (ADR 0070, ADR 0094). Some
service declarations are not scripts at all (`start: never`, the desktop), and
are written as a comment block in a file that is not otherwise a script.

## Decision

### 1. One declaration format, two shapes

A declaration directory holds files of two shapes, read by one package
(`declared`) for both services and tools:

- **`.yaml` / `.yml`** — the whole file is the metadata. Nothing is run *from*
  the file.
- **anything else** — a script with a front-matter block (`#---`, `//---`,
  `---`), which is itself what runs: `.sh`, `.py`, `.ps1`, ….

The id is the filename's, with the ordering prefix and extension removed
(`frontmatter.NormalizeID`, which now knows `.yaml`, `.yml`, `.ps1`), unless
`id:` states one. `name` and `description` mean the same thing everywhere.

A `.yaml` **service** has nothing to run, so `start: never` is implied and
`start: command` is a problem. The desktop becomes `10-desktop.yaml`.

### 2. A tool is a declaration in a `tools` directory

Four layers, **the last one wins on a shared id**:

| Layer | Directory | May declare |
| --- | --- | --- |
| builtin | embedded in the CLI | host tools |
| image | `/usr/local/share/discobox/tools` | sandbox tools |
| source | `<primary source>/.discobox/tools` | sandbox tools |
| user | `<user config dir>/discobox/tools` | both |

The image declares what it ships (`diff`, `fresh`); the CLI declares what runs
beside it (`vscode`, `zed`).

A tool a client has to *recognize* states its id, as the desktop service does.
The image's diff is `id: ai.discobox.diff` (`tools.DiffID`), which the launcher
opens from a discobox's git summary. Unlike the service namespace it is not
reserved: overriding it is the point, so a repository's or a person's
declaration of `ai.discobox.diff` — under any name — becomes the diff. A tool is
run by id, or by a name exactly one tool wears (`discobox tools diff`).

### 3. Where it runs, and what runs

`runs: sandbox` (the default) or `runs: host`.

| | `.yaml` | script |
| --- | --- | --- |
| **sandbox** | `program` + `args`, exec'd in the primary source directory | the script; by its path for image and source, written to a private temporary directory for the run for user |
| **host** | the first `program` on PATH (or `$<program-env>`, or `--program`) + `args` | the script; a `.ps1` through PowerShell |

In the TUI a sandbox tool is a labeled session (ADR 0071 §1–5, unchanged) and a
host tool is a request that returns; both are reached only from the tools
picker inside a discobox, not from the list of boxes. On the CLI both are
`discobox tools <id>`.

### 4. A host tool is handed the discobox

Before a host tool runs, the project's managed ssh_config is refreshed for
every ssh the program might use — the WSL rule of ADR 0102 decided by the
resolved program, as it was for the editors. Then:

| Placeholder in `args` | Environment for a script | Value |
| --- | --- | --- |
| `{ssh.host}` | `DISCOBOX_SSH_HOST` | the ssh_config host |
| `{workdir}` | `DISCOBOX_WORKDIR` | the working tree, `/` when none is known |
| `{workdir.urlpath}` | — | the same, escaped as a URL path |
| `{ssh.url}` | `DISCOBOX_SSH_URL` | `ssh://host/workdir` |
| `{git.url}` | `DISCOBOX_GIT_URL` | the same, and an error when there is no working tree |
| `{discobox.id}` | `DISCOBOX_ID` | the sandbox id |

`env:` adds `NAME=value` entries to either kind.

### 5. Only you and the CLI put a program on your machine

A `runs: host` declaration from the image or the source is listed with a
problem and never run. An image is chosen by whoever configured the harness
and a repository by whoever wrote it; either declaring a host tool would make
opening a discobox run their program on your laptop, outside the sandbox that
exists to contain them.

For the same reason an image or source declaration **cannot replace a host
tool** of a lower layer: it is listed with a problem, and the host tool stays.
What a key on your machine does is not the box's to change: the refused
declaration is listed but takes neither the host tool's row nor its key, and in
a picker the tools that run on this machine choose their keys first.

Everything a sandbox agent sends is checked again by the CLI (`Definition.Check`)
— an id or file name that is a path, a declaration with nothing to run, a host
tool — because anything with root in the box can answer that route.

### 6. Files ride with the declaration

`files:` maps a local name to where it lands under the run user's home:

```yaml
files:
  config.jsonc: .config/fresh/config.json
```

The default is `<dir>/<id>/<name>` beside the declaration, sent with it. The
local copy stays `<user config dir>/discobox/tools/<id>/<name>` — for a
user-declared tool, that is the default itself. Delivery is ADR 0071 §8–10
unchanged. A host tool carries no files.

A destination is a fixed path under the run user's home. ADR 0071 §11's
`{workspace}` — the working directory in fresh's own filename encoding — is
gone from delivery: it was one tool's rule in the generic mechanism. State keyed
on something only the discobox knows is the tool's own script's to write, so
fresh is `20-fresh.sh`, which records its trust decision (§12) and then execs
fresh.

### 7. The sandbox lists its layers; the CLI merges

`GET /api/projects/{p}/sandboxes/{s}/tools` returns the image and source
declarations, re-read on every request, gated like services. The CLI merges
them between its builtin and user layers. A sandbox agent that predates the
route is a box that declares no tools.

`discobox tools ls` shows the merged catalog with each declaration's layer and
problem. `git`, `ssh`, `ls` and `help` are the CLI's own names; a tool
declaring one is listed with a problem.

## Consequences

- vscode and zed are two `.yaml` files. `--editor` is `--program`,
  `$DISCOBOX_VSCODE` and `$DISCOBOX_ZED` still work, and `--reuse-window` is
  gone: a user-layer `vscode.yaml` saying `--reuse-window` is the setting.
- A VS Code window with no known working tree opens `/` rather than a bare
  remote, the rule Zed already had.
- `diff` and `fresh` gain CLI commands. They also disappear from boxes on an
  image older than the tools directory, until the box is upgraded; declaring
  them in the user layer brings them back.
- The picker's rows arrive after the box answers, as the address rows already
  did. A running tool session is still recognized by its label alone, so a
  reattach draws it before the catalog is known.
- A tool that runs on the host is limited to what argv and environment can
  say. Anything more is a script, which on Windows means `.ps1`.

## Alternatives rejected

- **Keep the Go table and add rows.** Every tool is a CLI release, the image
  cannot say what it ships, and a person cannot add their own.
- **Host tools as scripts only.** The binary search and the WSL rule need the
  resolved program before anything runs, a URL-escaped path is miserable in
  shell, and Windows has no `sh`. A `.yaml` host tool gets all three from the
  CLI; a script remains available when argv is not enough.
- **Let the image or source declare host tools, behind a prompt.** A prompt
  answered once per repository is a prompt answered without reading; the
  boundary is where the sandbox already draws it.
- **Source over user.** A repository's choice of reviewer is a reasonable
  default, but the person at the keyboard is the last word about their tools,
  as they are about a tool's config (ADR 0071 §7).
- **The CLI reads `.discobox/tools` with an exec.** Discovery, parsing and
  problems would live in two places. Services already put discovery in the
  sandbox agent behind a route.
