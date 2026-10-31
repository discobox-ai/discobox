# 26-10-30-584 — A credential policy file supplies a binding's routes

Status: Accepted
Date: 2026-10-30

## Context

A secret binding may narrow where a credential is sent using static routes
([ADR 26-10-27-880 §4](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)).
Writing those routes at each bind repeats permissions and makes service roles,
such as filing GitHub issues, difficult to share and review. A reusable file
should express those permissions independently of the secret and target.

## Decision

### 1. One JSON file describes one credential policy

The document has a required integer `version`, initially `1`, an optional
human-readable `description`, an optional `parameters` object, and a required,
nonempty `routes` array. Each
route uses the existing `SecretRoute` wire shape and matching semantics:
hosts, methods, paths, GraphQL operations and fields, MCP tools, and blocked
routes. There is no second policy language.

For example, `github-file-issues.json` permits creating issues in one repository:

```json
{
  "version": 1,
  "description": "Create issues in one repository",
  "parameters": {
    "owner": { "type": "pathSegment" },
    "repo": { "type": "pathSegment" }
  },
  "routes": [
    {
      "hosts": ["api.github.com"],
      "methods": ["POST"],
      "paths": ["/repos/${owner}/${repo}/issues"]
    }
  ]
}
```

The file contains permissions only. It names no secret, target, environment
variable, lifetime, raw delivery mode, or delegation right. Its filename is
how a person names the role; no role resource or registry is introduced.
A published JSON Schema provides editor validation and describes the same
versioned format; the CLI still validates every file it reads.

### 2. A binding reads a local file

At sandbox creation, the policy belongs to the individual `--bind` argument:

```sh
discobox run --bind 'github,policy=github-file-issues.json,owner=acme,repo=widget'
```

The existing secret and optional environment-variable selection remain
available, for example
`github:GITHUB_TOKEN,policy=github-file-issues.json,owner=acme,repo=widget`.
Binding to an existing target accepts the same document through `--policy`:

```sh
discobox secret bind github mybox --policy github-file-issues.json \
  --policy-arg owner=acme --policy-arg repo=widget
```

Paths are resolved relative to the invoking process's working directory.
The CLI reads and validates the file before submitting a mutation, then sends
its normalized routes through the existing binding API. The server receives
routes, not a path to open. No server-side filesystem or remote URL lookup is
introduced.

A policy file and inline routes are mutually exclusive on one binding. This
avoids an implicit union that could grant more access than either input
suggests. Existing bindings without a policy file keep their current behavior.

### 3. Parameters are typed and required unless explicitly optional

The `parameters` object maps names to declarations. Version 1 supports
`pathSegment`: a literal value substituted for `${name}` within a route path.
Arguments cannot supply path separators, wildcard characters, placeholder
syntax, or encoded forms that would escape the literal segment or broaden the
pattern. Parameters are not substituted into hosts, methods, protocol blocks,
or object keys. There are no expressions or recursive substitutions.

Parameters are required unless their declaration contains `"optional": true`.
Every optional parameter must carry an explicit `default`, including when that
default is `""`; optionality never implies an empty value. For example:

```json
{
  "type": "pathSegment",
  "optional": true,
  "default": ""
}
```

Supplied values and defaults undergo the same type and route validation. An
empty string is permitted only where both the parameter type and its use in
the resulting route permit it. It does not implicitly become a wildcard,
remove a route, or drop a restriction. Required parameters cannot declare a
default. Missing required arguments, unknown or duplicate arguments,
undeclared placeholders, and invalid declarations are errors.

Arguments belong to one binding. At creation they follow `policy=FILE` in
that binding's `--bind` argument; `secret bind` takes repeatable
`--policy-arg NAME=VALUE` flags alongside `--policy`. Argument names must not
collide with binding option names such as `policy`.

The CLI resolves arguments and defaults before normalizing and submitting the
concrete routes. For the example above, the binding receives
`/repos/acme/widget/issues`; enforcement needs no parameter interpreter.

### 4. Multiple policies merge into one route list

One binding may name multiple policy files. Each occurrence has its own
arguments, scoped to that occurrence, so the same file may be used twice for
separate repositories. At creation a new `policy=FILE` starts a new argument
scope:

```sh
discobox run --bind 'github,policy=issues.json,owner=acme,repo=widget,policy=pulls.json,owner=acme,repo=widget'
```

For an existing target, repeat `--policy`; each `--policy-arg` belongs to the
preceding `--policy` occurrence:

```sh
discobox secret bind github mybox \
  --policy issues.json --policy-arg owner=acme --policy-arg repo=widget \
  --policy pulls.json --policy-arg owner=acme --policy-arg repo=widget
```

Resolve and validate every policy independently, then concatenate their
concrete routes into one binding route list. This is a union of route
candidates, not an intersection of independent policies. Existing route
specificity and blocked-route rules apply across the entire merged list: a
blocked route from one file can refuse an equally specific allow from another,
while a more specific allow can reopen a broader block. File order gives no
precedence. Every policy must be valid before any mutation is submitted.
Inline routes remain mutually exclusive with policy files.

### 5. The binding holds a snapshot, subject to every existing restriction

The binding persists the concrete routes resolved from the file and arguments. Editing or moving the file
does not change an existing binding; changing its permissions requires an
explicit binding update that reapplies the document. The file path and
description carry no authority and are not required at runtime. Existing
binding history records the resulting routes.

Requests must still satisfy the secret's routes, the binding's routes, and
every parent delegation's routes. A file cannot widen any of those other
lists. Within its own list, the existing most-specific-route and blocked-route
rules apply unchanged. The policy governs requests carrying the credential;
it is not the sandbox's general network egress policy.

### 6. Invalid documents fail before binding

Reject unsupported versions, unknown fields at every level, duplicate JSON
object keys, malformed or trailing JSON, invalid routes, and absent or empty
`routes`. In particular, an empty file policy must never become an empty
binding route list: that list currently means inherit the secret's routes,
which could silently remove the intended restriction.

Route validation and normalization use the existing shared route rules. A
read or validation failure identifies the file and offending field or route
and submits no binding or sandbox creation request.

## Alternatives rejected

- **A live reference from the binding to a file.** It would make a local edit
  change authorization remotely, require file availability after binding, and
  add synchronization and revocation behavior. A snapshot makes the bind the
  explicit permission change and keeps its audit self-contained.
- **Server-managed named roles.** They would add a resource lifecycle and
  authorization model before reuse requires one. Version-controlled files
  already provide sharing and review across credentials and projects.
- **A new service-specific policy language.** The proxy already enforces the
  route model; another language would need translation and risk divergent
  meanings. The file packages the existing contract.
- **Combine file and inline routes.** A union could widen access; an
  intersection would introduce another route-list boundary for one binding.
  One source per binding keeps the result explicit.
- **Include binding configuration in the file.** Coupling permissions to a
  secret name, variable, or lifetime would make the same service role harder
  to reuse with different credentials and targets.

## Consequences

Service roles become portable files that can be reviewed and versioned beside
the work that uses them. Enforcement, persisted binding routes, and binding
audit retain their existing ownership and semantics. No database migration
or policy resource is required. File or argument changes must be reapplied explicitly to
each binding that should receive them.

## Deferred

Named catalogs and includes are deferred until real usage demonstrates policy
families that cannot be maintained clearly as standalone files with literal
path-segment parameters. Additional parameter types require a concrete use
case and validation rules that preserve their literal meaning. Expressions
and arbitrary text templating are deferred; version 1 substitutes only the
declared path-segment parameters.
