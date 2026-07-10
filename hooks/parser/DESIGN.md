# Hooks Parser Design

The parser package owns the on-disk hook definition format for Discobox hooks. It
converts files under `.discobox/hooks` into validated hook definitions used by
the daemon, scheduler, runner, and store.

## Discovery Scope

Discovery is repository-root relative:

```text
$GIT_ROOT/.discobox/hooks
```

Rules:

- Read files directly inside `.discobox/hooks`; do not recurse.
- Ignore directories.
- Ignore hidden files whose base name starts with `.`.
- Ignore the global hook ignore file, if present at `.discobox/hooks/ignore`.
- Derive hook IDs from filenames after removing common hook extensions and any
  leading numeric order prefix such as `90-`; `90-review-lint.sh` becomes
  `review-lint`.
- Sort discovered hooks deterministically by name, then ID.
- Return validation errors with the hook path and field name when possible.

## Hook File Shapes

Script hooks are executable files with a shebang, front matter, and command body:

```bash
#!/usr/bin/env bash
#---
# name: Go tests
# type: file
# pattern: "**/*.go"
#---
go test ./...
```

Prompt-style files may be parsed for compatibility, but native AI execution is
not part of the initial runner. If accepted, the parser should preserve the body
as `Prompt` and the validator should report that `engine: ai` requires a later
execution implementation or script adapter.

```markdown
---
name: Go review
type: file
engine: ai
pattern: "**/*.go"
---
Review changed Go files for correctness and test coverage.
```

## Front Matter Delimiters

Support the Discobot-compatible delimiter forms:

- plain: `---`
- shell/comment: `#---`
- slash/comment: `//---`

For script files, allow a shebang before the opening delimiter. Metadata lines
inside commented front matter may start with the same comment prefix:

```bash
#!/bin/bash
#---
# name: Format
# type: file
# pattern: "**/*.go"
#---
gofmt -w "$@"
```

Equivalent plain form:

```text
---
name: Format
type: file
pattern: "**/*.go"
---
```

The body starts after the closing delimiter. For script hooks, the body remains
in the original file and is executed by path; the parser does not need to return
script body contents. For prompt-style hooks, body text is returned as `Prompt`.

## Metadata Fields

Canonical fields:

| Field | Required | Applies to | Meaning |
| --- | --- | --- | --- |
| `name` | no | all | Human display name. Defaults to a readable filename-derived name. |
| `type` | yes | all | One of `session`, `file`, or `pre-commit`. |
| `description` | no | all | Human description for status output. |
| `engine` | no | all | One of `script`, `ai`, or `builtin`; default `script`. |
| `run_as` | no | script/session | Execution user hint, initially `user` or `root`; default `user`. |
| `blocking` | no | session | Whether startup should wait/block on this session hook. |
| `pattern` | file | file | Glob pattern for repository-relative changed paths. |
| `ignore` | no | file | Hook-specific glob exclusion. |
| `exclude` | no | file | Alias for `ignore`. |
| `phase` | no | file | Optional phase gate. Initially only `review` is valid if non-empty. |
| `subagent` | no | ai compatibility | Parsed for compatibility; not used by native runner initially. |
| `language_id` | lsp | lsp | LSP text document language ID sent in `didOpen` notifications. |
| `min_severity` | no | lsp | Lowest diagnostic severity that makes the hook fail: `error`, `warning`, `information`/`info`, or `hint`; default `hint`. |

Field aliases should be normalized where useful:

- `exclude` -> `ignore`
- `run-as` -> `run_as`

Boolean fields accept YAML boolean values. Unknown fields should either be
preserved in an extension map or ignored with a warning; do not fail discovery on
unknown fields unless the field conflicts with a known contract.

## Hook Types

### `session`

Runs when the session daemon starts.

Required fields:

- `type: session`

Optional fields:

- `blocking`
- `run_as`
- `description`

Session hooks do not require `pattern`.

### `file`

Runs after a debounced file-change batch contains at least one created, modified,
or deleted repository-relative path matching `pattern` and not matching ignores.

Required fields:

- `type: file`
- `pattern`

Optional fields:

- `ignore` / `exclude`
- `phase`

### `pre-commit`

Represents a hook that can run during explicit Git pre-commit integration.

Required fields:

- `type: pre-commit`

Pre-commit hooks are discovered and validated by the parser, but installation and
session selection are daemon/CLI design concerns.

## Engines

### `script`

Default engine. Script hooks must have:

- a shebang as the first line
- executable bit on Unix
- valid front matter

The runner executes the file by path from the Git root.

### `lsp`

LSP hooks are executable scripts that start a language server over stdio. They
must use `type: file`, declare a `pattern`, and set `language_id`. The daemon
starts the script as a long-lived language server client session and sends
matching file changes through LSP notifications. Diagnostics are persisted as
current hook diagnostics and update hook status directly; LSP hooks do not enter
the serial script hook queue.

### `ai`

Compatibility engine for Discobot-format prompt files. The parser can parse and
return AI hook definitions, including prompt body, but the initial standalone
Discobox runner does not execute AI hooks natively. AI workflows should be
implemented as script hooks that call external AI tools and exit with normal hook
status semantics.

### `builtin`

Reserved for future built-in hooks. The parser may accept `engine: builtin` only
for built-ins registered by code; arbitrary user files should not be allowed to
claim unknown built-ins without a registered implementation.

## IDs and Paths

Hook IDs are stable and filename-derived:

1. Take the base filename.
2. Remove the final extension for common script/text suffixes such as `.sh`,
   `.bash`, `.py`, `.js`, `.ts`, `.md`, and `.txt`.
3. Lowercase.
4. Replace non-alphanumeric runs with `-`.
5. Trim leading/trailing `-`.
6. If empty, fail validation.

Examples:

| Filename | Hook ID |
| --- | --- |
| `go-check.sh` | `go-check` |
| `01 Review Go.md` | `01-review-go` |
| `terraform.plan.bash` | `terraform-plan` |

Paths stored in hook definitions should include both:

- absolute path for execution
- repository-relative path for status output, e.g. `.discobox/hooks/go-check.sh`

## Pattern and Ignore Semantics

Patterns match repository-relative paths using slash separators. The intended
glob syntax should match Discobot/picomatch-style patterns where practical:

- `*.go`
- `**/*.go`
- `src/**/*.ts`
- `*.{ts,tsx}`
- `{package.json,pnpm*.yaml}`

File changes are considered only after Git ignored paths are removed by the
watcher/scheduler. Parser-level `ignore` is hook-specific and filters paths after
`pattern` matches.

The global ignore file lives at:

```text
.discobox/hooks/ignore
```

Format:

- one glob per line
- blank lines ignored
- lines beginning with `#` ignored
- no required support for gitignore negation in the initial parser

## Validation Summary

Fail discovery for a hook file when:

- front matter is missing or malformed
- `type` is missing or unsupported
- `engine` is unsupported
- `type: file` lacks `pattern`
- `phase` is non-empty and not `review`
- `engine: script` lacks a shebang
- `engine: script` is not executable on Unix
- the filename cannot produce a stable ID

Do not fail discovery merely because no hooks directory exists; return an empty
hook list.
